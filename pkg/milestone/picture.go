package milestone

import (
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/bits"
)

// picture describes the first slice of a coded picture.
type picture struct {
	ok    bool // slice header parsed
	b     bool // bi-predicted, so display order differs from decode order
	reset bool // POC restarts here (IDR)
	poc   int
}

// pictureParser tracks the parameter sets needed to read slice types and
// picture order counts. Only the most recent SPS/PPS is kept, which matches
// single-stream camera output.
type pictureParser struct {
	h265 bool

	haveSPS, havePPS  bool
	log2MaxFrameNum   uint8
	pocType           uint32
	log2MaxPocLsb     uint8
	frameMbsOnly      bool
	separatePlanes    bool
	outputFlag        bool
	extraSliceBits    uint8
	prevMsb, prevLsb  int
	havePrevReference bool

	interval float64 // H.264 VUI frame interval in ms, 0 if not signalled
}

// parse inspects an AVCC access unit.
func (p *pictureParser) parse(avcc []byte) picture {
	var pic picture
	for len(avcc) >= 4 {
		size := int(binary.BigEndian.Uint32(avcc))
		if size < 2 || size > len(avcc)-4 {
			break
		}
		nalu := avcc[4 : 4+size]
		avcc = avcc[4+size:]
		if p.h265 {
			if done := p.h265NALU(nalu, &pic); done {
				return pic
			}
		} else if done := p.h264NALU(nalu, &pic); done {
			return pic
		}
	}
	return pic
}

func (p *pictureParser) h264NALU(nalu []byte, pic *picture) bool {
	switch nalu[0] & 0x1F {
	case 7:
		p.h264SPS(rbsp(nalu[1:], 0))
	case 8:
		p.havePPS = true
	case 1, 5:
		if !p.haveSPS || !p.havePPS {
			return true
		}
		idr := nalu[0]&0x1F == 5
		r := bits.NewReader(rbsp(nalu[1:], 64))
		_ = r.ReadUEGolomb() // first_mb_in_slice
		sliceType := r.ReadUEGolomb()
		_ = r.ReadUEGolomb() // pic_parameter_set_id
		if p.separatePlanes {
			_ = r.ReadBits8(2)
		}
		_ = r.ReadBits(p.log2MaxFrameNum)
		if !p.frameMbsOnly && r.ReadBit() != 0 {
			return true // field pictures: leave timing to the recorder
		}
		if idr {
			_ = r.ReadUEGolomb() // idr_pic_id
		}
		pic.b = sliceType%5 == 1
		pic.reset = idr
		if p.pocType != 0 {
			// Types 1 and 2 imply output order follows a fixed pattern;
			// type 2 forbids reordering entirely.
			pic.ok = !r.EOF && !pic.b
			return true
		}
		lsb := int(r.ReadBits(p.log2MaxPocLsb))
		if r.EOF {
			return true
		}
		reference := nalu[0]&0x60 != 0
		pic.poc = p.order(lsb, idr, reference)
		pic.ok = true
		return true
	}
	return false
}

func (p *pictureParser) h264SPS(b []byte) {
	r := bits.NewReader(b)
	profile := r.ReadByte()
	_ = r.ReadUint16() // constraint flags, level
	_ = r.ReadUEGolomb()
	p.separatePlanes = false
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ReadUEGolomb()
		if chroma == 3 {
			p.separatePlanes = r.ReadBit() != 0
		}
		_ = r.ReadUEGolomb() // bit_depth_luma_minus8
		_ = r.ReadUEGolomb() // bit_depth_chroma_minus8
		_ = r.ReadBit()      // qpprime_y_zero_transform_bypass_flag
		if r.ReadBit() != 0 {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.ReadBit() != 0 {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}
	p.log2MaxFrameNum = uint8(r.ReadUEGolomb() + 4)
	p.pocType = r.ReadUEGolomb()
	switch p.pocType {
	case 0:
		p.log2MaxPocLsb = uint8(r.ReadUEGolomb() + 4)
	case 1:
		_ = r.ReadBit()
		_ = r.ReadSEGolomb()
		_ = r.ReadSEGolomb()
		for n := r.ReadUEGolomb(); n > 0 && !r.EOF; n-- {
			_ = r.ReadSEGolomb()
		}
	}
	_ = r.ReadUEGolomb() // max_num_ref_frames
	_ = r.ReadBit()      // gaps_in_frame_num_value_allowed_flag
	_ = r.ReadUEGolomb() // pic_width_in_mbs_minus1
	_ = r.ReadUEGolomb() // pic_height_in_map_units_minus1
	p.frameMbsOnly = r.ReadBit() != 0
	p.haveSPS = !r.EOF && p.log2MaxFrameNum <= 16 && p.log2MaxPocLsb <= 16
	p.interval = h264FrameInterval(r, p.frameMbsOnly)
}

// h264FrameInterval reads the VUI timing info that follows frame_mbs_only_flag,
// in milliseconds, or 0 when absent or implausible.
func h264FrameInterval(r *bits.Reader, frameMbsOnly bool) float64 {
	if !frameMbsOnly {
		_ = r.ReadBit() // mb_adaptive_frame_field_flag
	}
	_ = r.ReadBit() // direct_8x8_inference_flag
	if r.ReadBit() != 0 {
		for i := 0; i < 4; i++ {
			_ = r.ReadUEGolomb() // frame_crop offsets
		}
	}
	if r.ReadBit() == 0 { // vui_parameters_present_flag
		return 0
	}
	if r.ReadBit() != 0 && r.ReadByte() == 255 { // aspect_ratio_idc == Extended_SAR
		_ = r.ReadUint32()
	}
	if r.ReadBit() != 0 {
		_ = r.ReadBit() // overscan_appropriate_flag
	}
	if r.ReadBit() != 0 { // video_signal_type_present_flag
		_ = r.ReadBits8(4)
		if r.ReadBit() != 0 {
			_ = r.ReadUint24()
		}
	}
	if r.ReadBit() != 0 {
		_ = r.ReadUEGolomb()
		_ = r.ReadUEGolomb()
	}
	if r.ReadBit() == 0 { // timing_info_present_flag
		return 0
	}
	tick, scale := r.ReadUint32(), r.ReadUint32()
	if r.EOF || tick == 0 || scale == 0 {
		return 0
	}
	// Two ticks per frame for frame-coded video.
	interval := 2000 * float64(tick) / float64(scale)
	if interval < 1000.0/120 || interval > 10_000 {
		return 0
	}
	return interval
}

func skipScalingList(r *bits.Reader, size int) {
	last, next := int32(8), int32(8)
	for j := 0; j < size && !r.EOF; j++ {
		if next != 0 {
			next = (last + r.ReadSEGolomb() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

func (p *pictureParser) h265NALU(nalu []byte, pic *picture) bool {
	nalType := (nalu[0] >> 1) & 0x3F
	temporalID := nalu[1]&0x07 - 1
	switch {
	case nalType == 33:
		p.h265SPS(rbsp(nalu[2:], 0))
	case nalType == 34:
		p.h265PPS(rbsp(nalu[2:], 0))
	case nalType <= 21:
		if !p.haveSPS || !p.havePPS {
			return true
		}
		r := bits.NewReader(rbsp(nalu[2:], 64))
		if r.ReadBit() == 0 {
			return false // not the first slice segment of the picture
		}
		if nalType >= 16 && nalType <= 23 {
			_ = r.ReadBit() // no_output_of_prior_pics_flag
		}
		_ = r.ReadUEGolomb() // slice_pic_parameter_set_id
		for i := uint8(0); i < p.extraSliceBits; i++ {
			_ = r.ReadBit()
		}
		sliceType := r.ReadUEGolomb()
		if p.outputFlag {
			_ = r.ReadBit()
		}
		if p.separatePlanes {
			_ = r.ReadBits8(2)
		}
		idr := nalType == 19 || nalType == 20
		lsb := 0
		if !idr {
			lsb = int(r.ReadBits(p.log2MaxPocLsb))
		}
		if r.EOF {
			return true
		}
		// BLA and the first CRA also restart the count; cameras send IDRs.
		reset := idr || (nalType >= 16 && nalType <= 18)
		// Sub-layer non-reference, RADL and RASL pictures do not anchor the count.
		reference := temporalID == 0 && !(nalType <= 14 && nalType%2 == 0) && (nalType < 6 || nalType > 9)
		pic.b = sliceType == 0
		pic.reset = reset
		pic.poc = p.order(lsb, reset, reference)
		pic.ok = true
		return true
	}
	return false
}

func (p *pictureParser) h265SPS(b []byte) {
	r := bits.NewReader(b)
	_ = r.ReadBits8(4) // sps_video_parameter_set_id
	subLayers := r.ReadBits8(3)
	_ = r.ReadBit()
	// profile_tier_level: general profile (88 bits) and level (8 bits)
	_ = r.ReadBits64(48)
	_ = r.ReadBits64(48)
	profilePresent := make([]bool, subLayers)
	levelPresent := make([]bool, subLayers)
	for i := range profilePresent {
		profilePresent[i] = r.ReadBit() != 0
		levelPresent[i] = r.ReadBit() != 0
	}
	if subLayers > 0 {
		for i := subLayers; i < 8; i++ {
			_ = r.ReadBits8(2)
		}
	}
	for i := range profilePresent {
		if profilePresent[i] {
			_ = r.ReadBits64(44)
			_ = r.ReadBits64(44)
		}
		if levelPresent[i] {
			_ = r.ReadBits8(8)
		}
	}
	_ = r.ReadUEGolomb() // sps_seq_parameter_set_id
	p.separatePlanes = false
	if r.ReadUEGolomb() == 3 {
		p.separatePlanes = r.ReadBit() != 0
	}
	_ = r.ReadUEGolomb()  // pic_width_in_luma_samples
	_ = r.ReadUEGolomb()  // pic_height_in_luma_samples
	if r.ReadBit() != 0 { // conformance_window_flag
		for i := 0; i < 4; i++ {
			_ = r.ReadUEGolomb()
		}
	}
	_ = r.ReadUEGolomb() // bit_depth_luma_minus8
	_ = r.ReadUEGolomb() // bit_depth_chroma_minus8
	p.log2MaxPocLsb = uint8(r.ReadUEGolomb() + 4)
	p.haveSPS = !r.EOF && p.log2MaxPocLsb <= 16
}

func (p *pictureParser) h265PPS(b []byte) {
	r := bits.NewReader(b)
	_ = r.ReadUEGolomb() // pps_pic_parameter_set_id
	_ = r.ReadUEGolomb() // pps_seq_parameter_set_id
	_ = r.ReadBit()      // dependent_slice_segments_enabled_flag
	p.outputFlag = r.ReadBit() != 0
	p.extraSliceBits = r.ReadBits8(3)
	p.havePPS = !r.EOF
}

// order derives the picture order count from its LSB (H.264 8.2.1.1, H.265 8.3.1).
func (p *pictureParser) order(lsb int, reset, reference bool) int {
	if reset || !p.havePrevReference {
		p.prevMsb, p.prevLsb = 0, 0
	}
	maxLsb := 1 << p.log2MaxPocLsb
	msb := p.prevMsb
	switch {
	case lsb < p.prevLsb && p.prevLsb-lsb >= maxLsb/2:
		msb += maxLsb
	case lsb > p.prevLsb && lsb-p.prevLsb > maxLsb/2:
		msb -= maxLsb
	}
	if reference || reset {
		p.prevMsb, p.prevLsb = msb, lsb
		p.havePrevReference = true
	}
	return msb + lsb
}

// rbsp strips emulation prevention bytes, reading at most limit output bytes
// (0 = all). Slice headers only need the first few dozen bytes.
func rbsp(b []byte, limit int) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}
