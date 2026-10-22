package exacq

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
	"github.com/stretchr/testify/require"
)

type fakeMediaSession struct {
	inventory *native.Inventory
	live      []*native.MediaPacket
	batches   [][]*native.MediaPacket
	requests  [][2]time.Time
	selected  uint32
	closed    bool
}

func (s *fakeMediaSession) Inventory() *native.Inventory { return s.inventory }
func (s *fakeMediaSession) StartLive(id uint32) error    { s.selected = id; return nil }
func (s *fakeMediaSession) Close() error                 { s.closed = true; return nil }
func (s *fakeMediaSession) ReadMedia(ctx context.Context) (*native.MediaPacket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(s.live) == 0 {
		return nil, io.EOF
	}
	p := s.live[0]
	s.live = s.live[1:]
	return p, nil
}
func (s *fakeMediaSession) Search(ctx context.Context, id uint32, start, end time.Time) ([]*native.MediaPacket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.selected = id
	s.requests = append(s.requests, [2]time.Time{start, end})
	if len(s.batches) == 0 {
		return nil, nil
	}
	frames := append([]*native.MediaPacket(nil), s.batches[0]...)
	s.batches = s.batches[1:]
	return frames, nil
}

func fixtureMedia(t *testing.T, name string, format uint32) []*native.MediaPacket {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	aud := []byte{0, 0, 0, 1, 9}
	if format == 6 {
		aud[4] = 70
	}
	var positions []int
	for offset := 0; offset < len(data); {
		n := bytes.Index(data[offset:], aud)
		if n < 0 {
			break
		}
		positions = append(positions, offset+n)
		offset += n + len(aud)
	}
	require.NotEmpty(t, positions)
	positions = append(positions, len(data))
	var packets []*native.MediaPacket
	for i := 0; i+1 < len(positions); i++ {
		stamp := int64(1_700_000_000_000 + i*1000/15)
		p := &native.MediaPacket{StreamID: 17, Type: 2, Seconds: uint32(stamp / 1000), Milliseconds: uint16(stamp % 1000), Data: bytes.Clone(data[positions[i]:positions[i+1]])}
		if i%15 == 0 {
			p.Type = 1
		}
		packets = append(packets, p)
	}
	return packets
}

func mockProducer(t *testing.T, source *Source, format uint32, session *fakeMediaSession) *Producer {
	t.Helper()
	session.inventory = &native.Inventory{Streams: []native.Stream{{ID: 101, ParentCameraID: 101, WireID: 17, Format: format, Framerate: 15, Enabled: true}}}
	streams, err := streamCandidates(session.inventory, source)
	require.NoError(t, err)
	p := newProducer(source, session, streams[0], "recorder:22609")
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// annexBUnits splits a fixture access unit on its start codes.
func annexBUnits(data []byte) [][]byte {
	var units [][]byte
	for _, part := range bytes.Split(data, []byte{0, 0, 1}) {
		if part = bytes.TrimRight(part, "\x00"); len(part) != 0 {
			units = append(units, part)
		}
	}
	return units
}

// stripParameters removes SPS/PPS (and VPS) so a timed keyframe depends on
// parameter sets cached from the recorder's untimed buffer.
func stripParameters(t *testing.T, frame *native.MediaPacket, format uint32) {
	nalus := annexBUnits(frame.Data)
	require.NotEmpty(t, nalus)
	frame.Data = nil
	for _, nalu := range nalus {
		n := nalu[0] & 31
		if format == 5 && (n == 7 || n == 8) {
			continue
		}
		n = nalu[0] >> 1 & 63
		if format == 6 && n >= 32 && n <= 34 {
			continue
		}
		frame.Data = append(frame.Data, 0, 0, 0, 1)
		frame.Data = append(frame.Data, nalu...)
	}
}

func TestProducerLiveStartsFromBufferedGOP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
		codec  string
	}{{"ip.h264", 5, core.CodecH264}, {"ip.h265", 6, core.CodecH265}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			require.Len(t, frames, 30)
			// The recorder answers with the GOP so far, untimed, then continues it.
			var live []*native.MediaPacket
			for i, frame := range frames {
				copy := *frame
				if i < 5 {
					copy.Seconds, copy.Milliseconds = 0, 0
				}
				live = append(live, &copy)
			}
			p := mockProducer(t, mustParse(t, "exacq://user:password@recorder/101"), tc.format, &fakeMediaSession{live: live})
			require.NoError(t, p.probe(context.Background()))
			interval := time.Second / 15
			require.Equal(t, frames[5].Timestamp().Add(-5*interval), p.ActualStart, "back-dated from the first timed picture")
			require.Equal(t, tc.codec, p.video.Codec.Name)
			var output []core.SampleTiming
			p.video.Input = func(packet *core.Packet) {
				timing, _ := core.GetSampleTiming(packet)
				output = append(output, timing)
			}
			require.NoError(t, p.Start())
			require.Len(t, output, 30, "the buffered GOP is emitted, not discarded")
			for i := 1; i < len(output); i++ {
				require.Equal(t, output[i-1].DecodeTime+uint64(output[i-1].Duration), output[i].DecodeTime, "contiguous at %d", i)
			}
		})
	}
}

func TestProducerLiveDiscontinuousBufferWaitsForKeyframe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
		codec  string
	}{{"ip.h264", 5, core.CodecH264}, {"ip.h265", 6, core.CodecH265}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			cached := *frames[0]
			cached.Seconds, cached.Milliseconds = 0, 0
			stripParameters(t, frames[15], tc.format)
			// Pictures 1-4 are missing, so the buffer cannot be placed: output
			// waits for the next timed keyframe, which relies on cached parameters.
			session := &fakeMediaSession{live: append([]*native.MediaPacket{&cached}, frames[5:]...)}
			p := mockProducer(t, mustParse(t, "exacq://user:password@recorder/101"), tc.format, session)
			require.NoError(t, p.probe(context.Background()))
			require.Equal(t, frames[15].Timestamp(), p.ActualStart)
			require.Equal(t, tc.codec, p.video.Codec.Name)
			require.NotContains(t, p.URL, "password")
			var output []*core.Packet
			p.video.Input = func(packet *core.Packet) { output = append(output, packet) }
			require.NoError(t, p.Start())
			require.Len(t, output, 15, "the last picture is released when the session ends")
			for i, packet := range output[:14] {
				timing, ok := core.GetSampleTiming(packet)
				require.True(t, ok)
				delta := frames[i+16].Timestamp().Sub(frames[i+15].Timestamp())
				require.Equal(t, uint32(delta.Milliseconds()*90), timing.Duration)
				require.Zero(t, timing.CompositionOffset)
				if i > 0 {
					previous, _ := core.GetSampleTiming(output[i-1])
					require.Equal(t, previous.DecodeTime+uint64(previous.Duration), timing.DecodeTime, "samples must not overlap")
				}
			}
		})
	}
}

func mustParse(t *testing.T, raw string) *Source {
	t.Helper()
	source, err := ParseURL(raw)
	require.NoError(t, err)
	return source
}

func TestProducerPlaybackPrerollGapsOverlapAndEOF(t *testing.T) {
	frames := fixtureMedia(t, "ip.h264", 5)
	second := []*native.MediaPacket{frames[15], frames[16]} // duplicate preceding GOP
	for _, frame := range frames[:4] {
		copy := *frame
		copy.Seconds += 10
		second = append(second, &copy)
	}
	source, err := ParseURL("exacq://user:password@recorder/101?start=1700000000013&end=1700000010100&speed=32")
	require.NoError(t, err)
	session := &fakeMediaSession{batches: [][]*native.MediaPacket{frames, second}}
	p := mockProducer(t, source, 5, session)
	require.NoError(t, p.probe(context.Background()))
	require.Equal(t, frames[0].Timestamp(), p.ActualStart, "preceding keyframe is retained")
	require.True(t, p.video.Lossless)
	var output []*core.Packet
	p.video.Input = func(packet *core.Packet) { output = append(output, packet) }
	require.NoError(t, p.Start())
	require.Len(t, output, 32, "30 initial pictures plus two before exclusive end")
	require.Len(t, session.requests, 2)
	require.True(t, session.closed)
	select {
	case <-p.video.Done():
	default:
		t.Fatal("finite receiver did not end")
	}
	require.NoError(t, p.video.EndError())
	beforeGap, ok := core.GetSampleTiming(output[29])
	require.True(t, ok)
	afterGap, ok := core.GetSampleTiming(output[30])
	require.True(t, ok)
	require.Greater(t, beforeGap.Duration, uint32(20000), "recording gap must remain on output clock")
	require.Equal(t, uint64(28125), afterGap.DecodeTime, "10 second gap scaled by speed 32")
	require.Equal(t, beforeGap.DecodeTime+uint64(beforeGap.Duration), afterGap.DecodeTime)
	final, _ := core.GetSampleTiming(output[len(output)-1])
	require.LessOrEqual(t, final.DecodeTime+uint64(final.Duration), uint64(28407), "final duration respects requested end")
}

func TestProducerNearestPrerollIsStable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
	}{{"ip.h264", 5}, {"ip.h265", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			// Fractional keyframes exercise a second search rounded down to a
			// whole second, which can return another earlier GOP.
			for _, frame := range frames {
				stamp := frame.Timestamp().Add(137 * time.Millisecond).UnixMilli()
				frame.Seconds, frame.Milliseconds = uint32(stamp/1000), uint16(stamp%1000)
			}
			start := frames[20].Timestamp()
			actual, ok := native.PlaybackStart(frames, start)
			require.True(t, ok)
			require.Equal(t, frames[15].Timestamp(), actual)
			again, ok := native.PlaybackStart(frames, actual)
			require.True(t, ok)
			require.Equal(t, actual, again)
			for _, requestStart := range []time.Time{start, actual} {
				source, err := ParseURL("exacq://user:password@recorder/101?start=1700000000000&end=1700000010000")
				require.NoError(t, err)
				source.Start, source.End = requestStart, frames[29].Timestamp().Add(time.Millisecond)
				p := mockProducer(t, source, tc.format, &fakeMediaSession{batches: [][]*native.MediaPacket{frames}})
				require.NoError(t, p.probe(context.Background()))
				require.Equal(t, actual, p.ActualStart)
			}
			first, ok := native.PlaybackStart(frames, frames[0].Timestamp().Add(-time.Second))
			require.True(t, ok)
			require.Equal(t, frames[0].Timestamp(), first)
			_, ok = native.PlaybackStart([]*native.MediaPacket{nil, {Type: native.MediaIFrame}, frames[1]}, start)
			require.False(t, ok)
		})
	}
}

func TestProducerPlaybackStartsAfterEmptyWindow(t *testing.T) {
	frames := fixtureMedia(t, "ip.h264", 5)
	source, err := ParseURL("exacq://user:password@recorder/101?start=1699999980000&end=1700000001000")
	require.NoError(t, err)
	session := &fakeMediaSession{batches: [][]*native.MediaPacket{nil, nil, frames}}
	p := mockProducer(t, source, 5, session)
	require.NoError(t, p.probe(context.Background()))
	require.Equal(t, frames[0].Timestamp(), p.ActualStart)
	require.Len(t, session.requests, 3)
}

func TestProducerReordersBFrames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		format   uint32
		playback bool
	}{{"b.h264", 5, false}, {"b-closed.h265", 6, false}, {"b.h264", 5, true}, {"b-closed.h265", 6, true}} {
		t.Run(fmt.Sprintf("%s/playback=%v", tc.name, tc.playback), func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			for _, frame := range frames {
				frame.Type = native.MediaPFrame // B detection must not rely on the packet label
			}
			frames[0].Type, frames[15].Type, frames[30].Type = native.MediaIFrame, native.MediaIFrame, native.MediaIFrame
			url := "exacq://user:password@recorder/101"
			session := &fakeMediaSession{live: frames}
			if tc.playback {
				url += "?start=1700000000000&end=1700000010000&speed=32"
				session = &fakeMediaSession{batches: [][]*native.MediaPacket{frames}}
			}
			source, err := ParseURL(url)
			require.NoError(t, err)
			p := mockProducer(t, source, tc.format, session)
			require.NoError(t, p.probe(context.Background()))
			var output []core.SampleTiming
			p.video.Input = func(packet *core.Packet) {
				timing, ok := core.GetSampleTiming(packet)
				require.True(t, ok)
				output = append(output, timing)
			}
			require.NoError(t, p.Start())
			require.Len(t, output, 45)
			display := bFixtureDisplay[tc.name]
			for gop := 0; gop < 3; gop++ {
				pictures := output[gop*15 : gop*15+15]
				for i, timing := range pictures {
					if i > 0 {
						previous := output[gop*15+i-1]
						require.Equal(t, previous.DecodeTime+uint64(previous.Duration), timing.DecodeTime, "decode times are contiguous at %d", i)
					} else if gop > 0 {
						// Keyframes re-anchor to the recorder clock: never earlier.
						previous := output[gop*15-1]
						require.GreaterOrEqual(t, timing.DecodeTime, previous.DecodeTime+uint64(previous.Duration))
					}
					pts := timing.DecodeTime + uint64(timing.CompositionOffset)
					for j, other := range pictures {
						otherPTS := other.DecodeTime + uint64(other.CompositionOffset)
						require.Equal(t, display[i] < display[j], pts < otherPTS, "presentation follows display order (%d vs %d)", i, j)
					}
				}
			}
		})
	}
}

func TestPlaybackStopInterruptsGap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- waitUntil(ctx, time.Now().Add(time.Hour)) }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("pacing did not cancel")
	}
}

func FuzzVideoCodec(f *testing.F) {
	for _, fixture := range []struct {
		name   string
		format uint8
	}{{"ip.h264", 0}, {"ip.h265", 1}} {
		data, err := os.ReadFile(filepath.Join("testdata", fixture.name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, fixture.format)
	}
	f.Add([]byte{0, 0, 0, 1, 0x65, 0x80}, uint8(5))
	f.Add([]byte{0, 0, 0, 1, 0x26, 1, 0x80}, uint8(6))
	f.Fuzz(func(t *testing.T, data []byte, format uint8) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		v := native.VideoAssembler{Format: uint32(5 + format%2)}
		frame, err := v.Prepare(&native.MediaPacket{Seconds: 1, Type: 1, Data: data})
		if err != nil || frame == nil {
			return
		}
		for remaining := frame.Data; len(remaining) != 0; {
			if len(remaining) < 4 {
				t.Fatal("truncated AVCC output")
			}
			n := int(binary.BigEndian.Uint32(remaining))
			if n == 0 || n > len(remaining)-4 {
				t.Fatal("malformed AVCC output")
			}
			remaining = remaining[n+4:]
		}
		if strings.Contains(videoCodec(v.Format, frame.Data).FmtpLine, "\x00") {
			t.Fatal("invalid codec configuration")
		}
	})
}

// Decode index -> display index for the generated three-GOP B fixtures.
var bFixtureDisplay = map[string][]int{
	"b.h264":        {0, 3, 1, 2, 6, 4, 5, 9, 7, 8, 12, 10, 11, 14, 13},
	"b-closed.h265": {0, 4, 2, 1, 3, 8, 6, 5, 7, 12, 10, 9, 11, 14, 13},
}
