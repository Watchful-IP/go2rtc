package milestone

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strconv"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/stretchr/testify/require"
)

func liveFrames(t *testing.T, name string) []frame {
	rd := newReader(bytes.NewReader(fixture(t, name)))
	var out []frame
	for {
		res, err := rd.read()
		if err != nil {
			return out
		}
		if res.isXML() {
			continue
		}
		fs, err := (&stream{client: &Client{}}).frames(res)
		require.NoError(t, err)
		out = append(out, fs...)
	}
}

func TestPictureH264FromRecorder(t *testing.T) {
	fs := liveFrames(t, "fx264b_live.bin")
	require.Len(t, fs, 30)
	require.True(t, fs[0].key)

	var p pictureParser
	var bCount int
	gop := map[int]bool{}
	for i, f := range fs {
		pic := p.parse(annexb.EncodeToAVCC(f.data))
		require.True(t, pic.ok, "picture %d", i)
		require.Equal(t, f.key, pic.reset, "picture %d", i)
		if pic.b {
			bCount++
		}
		if f.key {
			gop = map[int]bool{}
		}
		require.False(t, gop[pic.poc], "unique POC within a GOP at %d", i)
		gop[pic.poc] = true
	}
	require.Greater(t, bCount, 15)
	require.InDelta(t, 100, p.interval, 0.01, "VUI timing: 10 fps")
}

// hevcAccessUnits splits an Annex-B HEVC stream on access unit delimiters.
func hevcAccessUnits(t *testing.T, b []byte) [][]byte {
	var units [][]byte
	var start = -1
	for i := 0; i+5 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 && (b[i+4]>>1)&0x3F == 35 {
			if start >= 0 {
				units = append(units, b[start:i])
			}
			start = i
		}
	}
	require.GreaterOrEqual(t, start, 0)
	return append(units, b[start:])
}

func TestPictureH265BFrames(t *testing.T) {
	// x265, 10 fps, 12-picture GOPs, three pyramid B-frames, no recorder involved.
	units := hevcAccessUnits(t, fixture(t, "h265b.hevc"))
	require.Len(t, units, 24)

	p := pictureParser{h265: true}
	var pocs []int
	bCount := 0
	for i, unit := range units {
		pic := p.parse(annexb.EncodeToAVCC(unit))
		require.True(t, pic.ok, "picture %d", i)
		if pic.b {
			bCount++
		}
		if i == 12 {
			require.True(t, pic.reset, "IDR restarts POC")
		}
		if i < 12 {
			pocs = append(pocs, pic.poc)
		}
	}
	require.Greater(t, bCount, 6)
	sorted := append([]int(nil), pocs...)
	sort.Ints(sorted)
	require.NotEqual(t, pocs, sorted, "decode order differs from output order")
	for i, poc := range sorted {
		require.Equal(t, i, poc, "POCs cover the GOP once")
	}
}

// gbdLive wraps access units the way the recorder sends live H.265: one
// GenericByteData packet per response, arriving in decode order in bursts.
func gbdLive(units [][]byte) []byte {
	var out bytes.Buffer
	t := int64(1_790_000_000_000)
	sync := t
	for i, unit := range units {
		key := bytes.Contains(unit, []byte{0, 0, 0, 1, 0x40, 0x01}) // carries a VPS: IRAP
		if key {
			sync = t + int64(i)*100
		}
		arrival := t + int64(i/4)*400 + int64(i%4) // mini-GOP bursts
		header := make([]byte, videoHeaderSize)
		binary.BigEndian.PutUint16(header, gbdVideoPacket)
		binary.BigEndian.PutUint32(header[2:], uint32(videoHeaderSize+len(unit)))
		binary.BigEndian.PutUint16(header[6:], codecH265)
		binary.BigEndian.PutUint16(header[8:], uint16(i))
		if key {
			binary.BigEndian.PutUint16(header[10:], flagSync)
		}
		binary.BigEndian.PutUint64(header[12:], uint64(sync))
		binary.BigEndian.PutUint64(header[20:], uint64(arrival))
		body := append(header, unit...)
		out.WriteString("ImageResponse\r\nRequestId: 2\r\nCurrent: " + strconv.FormatInt(arrival, 10) +
			"\r\nContent-length: " + strconv.Itoa(len(body)) + "\r\nContent-type: application/x-genericbytedata-octet-stream\r\n\r\n")
		out.Write(body)
		out.WriteString("\r\n\r\n")
	}
	return out.Bytes()
}

func TestLiveH265BFrames(t *testing.T) {
	units := hevcAccessUnits(t, fixture(t, "h265b.hevc"))
	rec := newFakeRecorder(t)
	rec.live = gbdLive(units)

	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	codec := prod.GetMedias()[0].Codecs[0]
	require.Equal(t, core.CodecH265, codec.Name)
	require.Contains(t, codec.FmtpLine, "sprop-vps=")

	frames, err := collect(t, prod)
	require.Error(t, err)
	require.GreaterOrEqual(t, len(frames), 24-reorderWindow)
	for i, f := range frames {
		require.True(t, f.timed)
		require.GreaterOrEqual(t, f.pts, f.dts, "composition offset at %d", i)
	}
	for _, step := range presentationSteps(frames) {
		require.InDelta(t, 100, step, 1, "four pictures per 400 ms burst")
	}
}

func TestLiveBFramesAppearingMidStream(t *testing.T) {
	// Six pictures without B-frames decide the stream, then B-frames appear
	// mid-GOP; reordering must wait for the next keyframe.
	plain := hevcAccessUnits(t, fixture(t, "h265.hevc"))
	bframes := hevcAccessUnits(t, fixture(t, "h265b.hevc"))
	units := append(append(plain[:6:6], bframes[1:12]...), bframes[12:]...)

	rec := newFakeRecorder(t)
	rec.live = gbdLive(units)
	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	frames, _ := collect(t, prod)
	require.GreaterOrEqual(t, len(frames), len(units)-reorderWindow)

	for i, f := range frames {
		require.Equal(t, i >= 17, f.timed, "reordered from the second keyframe only, at %d", i)
		require.GreaterOrEqual(t, f.pts, f.dts, "composition offset at %d", i)
		if i > 0 {
			require.Greater(t, f.dts, frames[i-1].dts, "decode order at %d", i)
		}
	}
}
