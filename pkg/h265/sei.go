package h265

import (
	"bytes"
	"encoding/binary"
)

// filterSEI removes malformed metadata from an assembled access unit in place.
// Uniview firmware can count emulation-prevention bytes in SEI payloadSize,
// making otherwise intact video fail in decoders that reject the SEI overrun.
func filterSEI(au []byte) []byte {
	out := au[:0]
	for len(au) > 0 {
		size := 4 + int(binary.BigEndian.Uint32(au))
		nalu := au[4:size]
		typ := (nalu[0] >> 1) & 0x3F
		if (typ != NALUTypePrefixSEI && typ != NALUTypeSuffixSEI) || validSEI(nalu) {
			out = append(out, au[:size]...)
		}
		au = au[size:]
	}
	return out
}

// validSEI checks message boundaries without interpreting vendor payloads.
func validSEI(nalu []byte) bool {
	if len(nalu) < 5 {
		return false
	}
	// SEI lengths describe RBSP bytes, before insertion of prevention bytes.
	rbsp := bytes.ReplaceAll(nalu[2:], []byte{0, 0, 3}, []byte{0, 0})
	for len(rbsp) > 1 {
		// payloadType can span multiple bytes; unknown types remain untouched.
		for len(rbsp) > 0 && rbsp[0] == 0xFF {
			rbsp = rbsp[1:]
		}
		if len(rbsp) < 2 {
			return false
		}
		rbsp = rbsp[1:]
		size := 0
		for {
			if len(rbsp) == 0 {
				return false
			}
			b := rbsp[0]
			rbsp = rbsp[1:]
			size += int(b)
			if size >= len(rbsp) { // reserve rbsp_trailing_bits
				return false
			}
			if b != 0xFF {
				break
			}
		}
		rbsp = rbsp[size:]
	}
	return len(rbsp) == 1 && rbsp[0] == 0x80
}
