package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".mp4")
	require.NoError(t, err)
	return b
}

func TestFFprobeOracle(t *testing.T) {
	for _, name := range []string{"h264", "h265"} {
		t.Run(name, func(t *testing.T) {
			data := fixture(t, name)
			var oracle struct {
				Packets []struct {
					StreamIndex int `json:"stream_index"`
					DTS, PTS    uint64
					DataHash    string `json:"data_hash"`
				}
				Streams []struct {
					Index     int
					ID        string
					TimeBase  string `json:"time_base"`
					CodecName string `json:"codec_name"`
				}
			}
			b, err := os.ReadFile("testdata/" + name + ".json")
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(b, &oracle))
			d := &Demuxer{}
			medias, err := d.Probe(data)
			require.NoError(t, err)
			require.Len(t, medias, 2)
			samples, err := d.Demux(data)
			require.NoError(t, err)
			require.Len(t, samples, len(oracle.Packets))
			streams := make(map[int]uint32)
			for _, s := range oracle.Streams {
				id, err := strconv.ParseUint(s.ID, 0, 32)
				require.NoError(t, err)
				streams[s.Index] = uint32(id)
			}
			byTrack := make(map[uint32][]Sample)
			for _, s := range samples {
				byTrack[s.TrackID] = append(byTrack[s.TrackID], s)
			}
			for _, p := range oracle.Packets {
				id := streams[p.StreamIndex]
				require.NotEmpty(t, byTrack[id])
				s := byTrack[id][0]
				byTrack[id] = byTrack[id][1:]
				require.Equal(t, p.DTS, s.DecodeTime)
				require.Equal(t, scaleTimestamp(p.PTS, d.tracks[id].codec.ClockRate, s.TimeScale), s.Packet.Timestamp)
				hash := sha256.Sum256(s.Packet.Payload)
				require.Equal(t, strings.ToLower(p.DataHash), "sha256:"+hex.EncodeToString(hash[:]))
			}
			bs, err := boxes(data, 0, len(data))
			require.NoError(t, err)
			n := 0
			for _, b := range bs {
				if b.name == "moof" {
					n++
				}
			}
			require.GreaterOrEqual(t, n, 11)
		})
	}
}

func atom(name string, parts ...[]byte) []byte {
	b := make([]byte, 8)
	copy(b[4:], name)
	for _, p := range parts {
		b = append(b, p...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}
func words(v ...uint32) []byte {
	var b []byte
	for _, n := range v {
		b = binary.BigEndian.AppendUint32(b, n)
	}
	return b
}
func testDemuxer() *Demuxer {
	return &Demuxer{tracks: map[uint32]*track{1: {codec: &core.Codec{Name: core.CodecAAC, ClockRate: 48000}, scale: 48000, description: 1, duration: 1024, size: 2}, 2: {codec: &core.Codec{Name: core.CodecAAC, ClockRate: 48000}, scale: 48000, description: 1, duration: 1024, size: 2}}}
}

func TestOffsetsAndDefaults(t *testing.T) {
	// Two trafs share an mdat; the second inherits the end of the first traf.
	tfdt := atom("tfdt", words(0, 48000))
	traf1 := atom("traf", atom("tfhd", words(1, 1, 0, 0)), tfdt, atom("trun", words(0, 1)), atom("trun", words(0, 1)))
	traf2 := atom("traf", atom("tfhd", words(0, 2)), tfdt, atom("trun", words(0, 1)))
	moof := atom("moof", traf1, traf2)
	binary.BigEndian.PutUint64(moof[32:], uint64(len(moof)+8))
	samples, err := testDemuxer().Demux(append(moof, atom("mdat", []byte{1, 2, 3, 4, 5, 6})...))
	require.NoError(t, err)
	require.Len(t, samples, 3)
	require.Equal(t, []byte{3, 4}, samples[1].Packet.Payload)
	require.Equal(t, uint64(49024), samples[1].DecodeTime)
	require.Equal(t, uint32(2), samples[2].TrackID)
	require.Equal(t, []byte{5, 6}, samples[2].Packet.Payload)
}

func TestSignedOffsetAndExtendedMdat(t *testing.T) {
	// An explicit base may point after the samples, with a negative trun offset.
	mdat := append(words(1), []byte("mdat")...)
	mdat = append(mdat, words(0, 18)...)
	mdat = append(mdat, 7, 8)
	tfhd := atom("tfhd", words(1|8|16|2, 1, 0, 18, 1, 2048, 2))
	moof := atom("moof", atom("traf", tfhd, atom("tfdt", words(1<<24, 1, 100)), atom("trun", words(1, 1, 0xfffffffe))))
	samples, err := testDemuxer().Demux(append(mdat, moof...))
	require.NoError(t, err)
	require.Len(t, samples, 1)
	require.Equal(t, []byte{7, 8}, samples[0].Packet.Payload)
	require.Equal(t, uint64(1<<32|100), samples[0].DecodeTime)
	require.Equal(t, uint32(2048), samples[0].Duration)
}

func TestMalformedFragments(t *testing.T) {
	for _, data := range [][]byte{nil, {0}, {0, 0, 0, 4, 'm', 'o', 'o', 'f'}, atom("moof", atom("traf")), atom("moof", atom("traf", atom("tfhd", words(0, 1)), atom("tfdt", words(0, 0)), atom("trun", words(0, 0xffffffff))))} {
		_, err := testDemuxer().Demux(data)
		require.Error(t, err)
	}
	data := fixture(t, "h264")
	d := &Demuxer{}
	_, err := d.Probe(data)
	require.NoError(t, err)
	for i := 0; i < len(data); i += 37 {
		_, _ = d.Demux(data[:i])
	}
}

func TestNALLengths(t *testing.T) {
	for _, n := range []int{1, 2, 4} {
		data := make([]byte, n)
		data[n-1] = 2
		data = append(data, 0x65, 0x80)
		got, err := normalizeNALs(data, n, 1)
		require.NoError(t, err)
		require.Equal(t, []byte{0, 0, 0, 2, 0x65, 0x80}, got)
	}
	_, err := normalizeNALs([]byte{0, 0, 0, 7, 0x65}, 4, 1)
	require.Error(t, err)
}

func TestTimestampPrecision(t *testing.T) {
	ts := uint64(1)<<54 | 12345
	require.Equal(t, uint32(ts/48000)*90000+uint32(ts%48000*90000/48000), scaleTimestamp(ts, 90000, 48000))
}

func FuzzDemux(f *testing.F) {
	data := fixture(f, "h264")
	f.Add(data)
	f.Add([]byte{0, 0, 0, 0, 'm', 'o', 'o', 'f'})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		d := testDemuxer()
		_, _ = d.Demux(data)
	})
}
func FuzzProbe(f *testing.F) {
	for _, name := range []string{"h264", "h265"} {
		data := fixture(f, name)
		bs, err := boxes(data, 0, len(data))
		require.NoError(f, err)
		for _, b := range bs {
			if b.name == "moof" {
				f.Add(data[:b.start])
				break
			}
		}
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		_, _ = (&Demuxer{}).Probe(data)
	})
}

func TestMetadataTrackWithoutMediaHeader(t *testing.T) {
	data := fixture(t, "h264")
	bs, err := boxes(data, 0, len(data))
	require.NoError(t, err)
	metadata := atom("trak", atom("tkhd", words(0, 0, 0, 3)), atom("mdia", atom("hdlr", words(0, 0), []byte("null"))))
	var init []byte
	for _, b := range bs {
		if b.name == "moov" {
			init = append(init, atom("moov", b.data, metadata)...)
		} else {
			init = append(init, data[b.start:b.end]...)
		}
	}
	d := &Demuxer{}
	medias, err := d.Probe(init)
	require.NoError(t, err)
	require.Len(t, medias, 2)
	require.Nil(t, d.tracks[3].codec)
	samples, err := d.Demux(data)
	require.NoError(t, err)
	require.NotEmpty(t, samples)
}

func TestSampleEntryAliases(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{{"h264", "avc1", "avc3"}, {"h265", "hvc1", "hev1"}} {
		data := bytes.Replace(fixture(t, tc.name), []byte(tc.from), []byte(tc.to), 1)
		medias, err := (&Demuxer{}).Probe(data)
		require.NoError(t, err)
		require.Len(t, medias, 2)
	}
}

func TestCompositionOffsetLimit(t *testing.T) {
	for _, cts := range []uint32{0xffffffff, 100000} {
		tfhd := atom("tfhd", words(1, 1, 0, 0))
		moof := atom("moof", atom("traf", tfhd, atom("tfdt", words(0, 0)), atom("trun", words(1<<24|0x800, 1, cts))))
		binary.BigEndian.PutUint64(moof[32:], uint64(len(moof)+8))
		_, err := testDemuxer().Demux(append(moof, atom("mdat", []byte{1, 2})...))
		require.ErrorContains(t, err, "composition offset")
	}
}

func TestMdatBoundaryIsEnforced(t *testing.T) {
	moof := atom("moof", atom("traf", atom("tfhd", words(1, 1, 0, 0)), atom("tfdt", words(0, 0)), atom("trun", words(0, 1))))
	// The sample would fit the segment but points into the mdat header.
	binary.BigEndian.PutUint64(moof[32:], uint64(len(moof)))
	_, err := testDemuxer().Demux(append(moof, atom("mdat", []byte{1, 2})...))
	require.ErrorContains(t, err, "outside mdat")
}

func TestTrexDefaultsFromInit(t *testing.T) {
	data := fixture(t, "h264")
	bs, err := boxes(data, 0, len(data))
	require.NoError(t, err)
	var init []byte
	for _, b := range bs {
		if b.name != "moov" {
			continue
		}
		cs, err := children(data, b)
		require.NoError(t, err)
		var parts [][]byte
		for _, c := range cs {
			if c.name != "mvex" {
				parts = append(parts, data[c.start:c.end])
			}
		}
		parts = append(parts, atom("mvex", atom("trex", words(0, 1, 1, 1024, 6, 0)), atom("trex", words(0, 2, 1, 1024, 2, 0))))
		init = atom("moov", parts...)
	}
	d := &Demuxer{}
	_, err = d.Probe(init)
	require.NoError(t, err)
	moof := atom("moof", atom("traf", atom("tfhd", words(0x020000, 1)), atom("tfdt", words(0, 100)), atom("trun", words(1, 2, 0))))
	binary.BigEndian.PutUint32(moof[len(moof)-4:], uint32(len(moof)+8))
	payload := []byte{0, 0, 0, 2, 0x65, 0x80}
	samples, err := d.Demux(append(moof, atom("mdat", payload, payload)...))
	require.NoError(t, err)
	require.Len(t, samples, 2)
	require.Equal(t, uint64(1124), samples[1].DecodeTime)
	require.Equal(t, uint32(1024), samples[1].Duration)
	require.Equal(t, payload, samples[1].Packet.Payload)
}

func BenchmarkDemux(b *testing.B) {
	for _, name := range []string{"h264", "h265"} {
		b.Run(name, func(b *testing.B) {
			data := fixture(b, name)
			d := &Demuxer{}
			_, err := d.Probe(data)
			require.NoError(b, err)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				_, err := d.Demux(data)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
