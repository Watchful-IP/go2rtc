package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func TestProgressivePacketOracle(t *testing.T) {
	var oracle struct {
		Packets []struct {
			StreamIndex int `json:"stream_index"`
			DTS, PTS    uint64
			Duration    uint32
			DataHash    string `json:"data_hash"`
		}
		Streams []struct {
			TimeBase string `json:"time_base"`
		}
	}
	for _, name := range []string{"progressive", "progressive-fast", "progressive-hevc"} {
		t.Run(name, func(t *testing.T) {
			oracleName := name
			if name == "progressive-fast" {
				oracleName = "progressive"
			}
			b, err := os.ReadFile("testdata/" + oracleName + ".json")
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(b, &oracle))
			data := fixture(t, name)
			p, err := OpenFile(bytes.NewReader(data), int64(len(data)))
			require.NoError(t, err)
			defer p.Stop()
			packets := make([][]*core.Packet, len(p.Medias))
			for i, m := range p.Medias {
				r, err := p.GetTrack(m, m.Codecs[0])
				require.NoError(t, err)
				r.Input = func(packet *core.Packet) { packets[i] = append(packets[i], packet) }
			}
			require.NoError(t, p.Start())
			for _, expected := range oracle.Packets {
				i := expected.StreamIndex
				require.NotEmpty(t, packets[i])
				packet := packets[i][0]
				packets[i] = packets[i][1:]
				scale, err := strconv.Atoi(strings.TrimPrefix(oracle.Streams[i].TimeBase, "1/"))
				require.NoError(t, err)
				clock := p.Medias[i].Codecs[0].ClockRate
				timing, ok := core.GetSampleTiming(packet)
				require.True(t, ok)
				require.Equal(t, expected.DTS*uint64(clock)/uint64(scale), timing.DecodeTime)
				require.Equal(t, uint32(expected.PTS*uint64(clock)/uint64(scale)), packet.Timestamp)
				end := (expected.DTS + uint64(expected.Duration)) * uint64(clock) / uint64(scale)
				require.Equal(t, uint32(end-timing.DecodeTime), timing.Duration)
				hash := sha256.Sum256(packet.Payload)
				require.Equal(t, strings.ToLower(expected.DataHash), "sha256:"+hex.EncodeToString(hash[:]))
			}
			for _, packets := range packets {
				require.Empty(t, packets)
			}
		})
	}
}

func remuxFile(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	st, err := f.Stat()
	require.NoError(t, err)
	p, err := OpenFile(f, st.Size())
	require.NoError(t, err)
	defer p.Stop()
	c := NewConsumer(nil)
	defer c.Stop()
	for _, m := range p.Medias {
		r, err := p.GetTrack(m, m.Codecs[0])
		require.NoError(t, err)
		require.NoError(t, c.AddTrack(m, m.Codecs[0], r))
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := c.WriteTo(&out); done <- err }()
	require.NoError(t, p.Start())
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(time.Second):
		t.Fatal("finite output did not complete")
	}
	for _, s := range c.Senders {
		require.Zero(t, s.Drops)
	}
	return out.Bytes()
}

func TestProgressiveRemux(t *testing.T) {
	out := remuxFile(t, "testdata/progressive.mp4")
	d := &Demuxer{}
	_, err := d.Probe(out)
	require.NoError(t, err)
	samples, err := d.Demux(out)
	require.NoError(t, err)
	// All 11 video and 53 AAC packets, including the final short AAC packet.
	require.Len(t, samples, 64)
	data := fixture(t, "progressive")
	p, err := OpenFile(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	defer p.Stop()
	expected := map[uint32][]core.SampleTiming{}
	for i, track := range p.tracks {
		for _, s := range track.samples {
			expected[uint32(i+1)] = append(expected[uint32(i+1)], s.timing)
		}
	}
	for _, s := range samples {
		e := expected[s.TrackID][0]
		expected[s.TrackID] = expected[s.TrackID][1:]
		actual, _ := core.GetSampleTiming(s.Packet)
		require.Equal(t, e.DecodeTime, actual.DecodeTime)
		require.Equal(t, e.Duration, actual.Duration)
		require.Equal(t, e.CompositionOffset, actual.CompositionOffset)
	}
	for _, remain := range expected {
		require.Empty(t, remain)
	}
}

func TestLVTFile(t *testing.T) {
	path := os.Getenv("GO2RTC_TEST_MP4")
	if path == "" {
		t.Skip("set GO2RTC_TEST_MP4 for a private archive fixture")
	}
	out := remuxFile(t, path)
	d := &Demuxer{}
	_, err := d.Probe(out)
	require.NoError(t, err)
	samples, err := d.Demux(out)
	require.NoError(t, err)
	require.Len(t, samples, 240)
	if path := os.Getenv("GO2RTC_TEST_MP4_OUTPUT"); path != "" {
		require.NoError(t, os.WriteFile(path, out, 0600))
	}
}

func FuzzProgressiveIndex(f *testing.F) {
	f.Add(fixture(f, "progressive"))
	f.Add(fixture(f, "progressive-fast"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		p, err := OpenFile(bytes.NewReader(data), int64(len(data)))
		if err == nil {
			p.Stop()
		}
	})
}
