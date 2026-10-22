package reorder

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/bits"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/stretchr/testify/require"
)

// Display indices were established independently by FFmpeg's decoded-frame
// pkt_pos values. All three GOPs are closed; no wire timestamps are synthesized.
func TestPictureOrderMatchesDecodedBFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hevc    bool
		scale   int
		display []int
		types   string
	}{
		{"b.h264", false, 2, []int{0, 3, 1, 2, 6, 4, 5, 9, 7, 8, 12, 10, 11, 14, 13}, "IPBBPBBPBBPBBPB"},
		{"b-closed.h265", true, 1, []int{0, 4, 2, 1, 3, 8, 6, 5, 7, 12, 10, 9, 11, 14, 13}, "IPBBBPBBBPBBBPB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units := fixtureUnits(t, tc.name, tc.hevc)
			require.Len(t, units, 45)
			parser := Parser{H265: tc.hevc}
			for i, unit := range units {
				pic := parser.Parse(annexb.EncodeToAVCC(unit))
				require.True(t, pic.OK, "frame %d", i)
				require.Equal(t, tc.display[i%15]*tc.scale, pic.POC, "frame %d", i)
				require.Equal(t, tc.types[i%15] == 'B', pic.B, "frame %d", i)
				require.Equal(t, i%15 == 0, pic.Reset, "frame %d", i)
			}
		})
	}
}

func TestH264BDetectionDoesNotRequireProgressivePOC(t *testing.T) {
	// first_mb=0, slice_type=B, pps=0, frame_num=0, field_pic_flag=1.
	w := bits.NewWriter(nil)
	pictureUE(w, 0)
	pictureUE(w, 1)
	pictureUE(w, 0)
	w.WriteBits8(0, 4)
	w.WriteBit(1)
	w.WriteBit(1)
	nalu := append([]byte{0x01}, w.Bytes()...)
	for _, p := range []Parser{
		{},
		{haveSPS: true, havePPS: true, frameMbsOnly: false, log2MaxFrameNum: 4, log2MaxPocLsb: 4},
		{haveSPS: true, havePPS: true, frameMbsOnly: true, log2MaxFrameNum: 4, pocType: 1},
	} {
		pic := p.Parse(h264.JoinNALU(nalu))
		require.True(t, pic.B, "unsupported POC must not disable the B-frame guard")
		require.False(t, pic.OK)
	}
}

func TestPictureRejectsOverflowingParameterWidths(t *testing.T) {
	for _, width := range []uint32{13, 252, 256, 1 << 20} {
		w := bits.NewWriter(nil)
		w.WriteByte(66)
		w.WriteUint16(30)
		pictureUE(w, 0)
		pictureUE(w, width)
		// Complete enough of a baseline SPS that uint8 truncation cannot hide
		// an invalid width behind an unrelated end-of-buffer failure.
		pictureUE(w, 0)
		pictureUE(w, 0)
		pictureUE(w, 1)
		w.WriteBit(0)
		pictureUE(w, 9)
		pictureUE(w, 5)
		w.WriteBit(1)
		w.WriteBit(1)
		w.WriteBit(0)
		w.WriteBit(0)
		p := Parser{}
		p.h264SPS(w.Bytes())
		require.False(t, p.haveSPS)
	}
}

func pictureUE(w *bits.Writer, v uint32) {
	v++
	n := uint8(0)
	for b := v; b > 1; b >>= 1 {
		n++
	}
	w.WriteAllBits(0, n)
	w.WriteBits(v, n+1)
}

func FuzzPictureParser(f *testing.F) {
	for _, b := range [][]byte{{0, 0, 0, 2, 0x01, 0xa8}, {0, 0, 0, 2, 0x67, 0xff}, {0, 0, 0, 2, 0x42, 1}} {
		f.Add(b, true)
		f.Add(b, false)
	}
	f.Fuzz(func(t *testing.T, data []byte, hevc bool) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		p := Parser{H265: hevc}
		p.Parse(data)
		if len(data) > 4 {
			copyData := append([]byte(nil), data...)
			binary.BigEndian.PutUint32(copyData, uint32(len(copyData)-4))
			p.Parse(copyData)
		}
	})
}

// fixtureUnits splits a generated Annex B fixture at its access unit delimiters.
func fixtureUnits(t *testing.T, name string, hevc bool) [][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	aud := []byte{0, 0, 0, 1, 9}
	if hevc {
		aud[4] = 70
	}
	var units [][]byte
	for len(data) > 0 {
		next := bytes.Index(data[1:], aud)
		if next < 0 {
			units = append(units, data)
			break
		}
		units = append(units, data[:next+1])
		data = data[next+1:]
	}
	return units
}

func TestPOCType2FollowsFrameNum(t *testing.T) {
	// Baseline SPS: log2_max_frame_num 4, poc type 2.
	sps := bits.NewWriter(nil)
	sps.WriteByte(66)
	sps.WriteUint16(30)
	pictureUE(sps, 0) // sps id
	pictureUE(sps, 0) // log2_max_frame_num_minus4
	pictureUE(sps, 2) // pic_order_cnt_type
	pictureUE(sps, 1) // max_num_ref_frames
	sps.WriteBit(0)
	pictureUE(sps, 9)
	pictureUE(sps, 5)
	sps.WriteBit(1) // frame_mbs_only
	sps.WriteBit(1)
	sps.WriteBit(0)
	sps.WriteBit(0)
	slice := func(idr bool, frameNum uint8) []byte {
		w := bits.NewWriter(nil)
		pictureUE(w, 0) // first_mb
		pictureUE(w, 0) // P (I for IDR is irrelevant to POC)
		pictureUE(w, 0) // pps
		w.WriteBits8(frameNum, 4)
		if idr {
			pictureUE(w, 0)
		}
		w.WriteBits8(0, 8)
		header := byte(0x41)
		if idr {
			header = 0x65
		}
		return append([]byte{header}, w.Bytes()...)
	}
	p := Parser{}
	p.Parse(h264.JoinNALU(append([]byte{0x67}, sps.Bytes()...), []byte{0x68, 0xce}))
	var pocs []int
	for i := 0; i < 20; i++ {
		pic := p.Parse(h264.JoinNALU(slice(i == 0, uint8(i%16))))
		require.True(t, pic.OK)
		pocs = append(pocs, pic.POC)
	}
	for i := 1; i < len(pocs); i++ {
		require.Equal(t, pocs[i-1]+2, pocs[i], "frame_num wrap must keep POC increasing at %d", i)
	}
}
