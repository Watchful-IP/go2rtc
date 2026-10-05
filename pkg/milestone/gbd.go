package milestone

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// GenericByteData data types and codec IDs, big-endian.
// https://doc.developer.milestonesys.com/mipsdk/reference/protocols/GenericByteData.html
const (
	gbdVideoPacket = 0x0010
	gbdAudioPacket = 0x0020
	gbdMetadata    = 0x0030
	gbdMultiPacket = 0xFEF0

	codecJPEG = 0x0001
	codecH264 = 0x000A
	codecH265 = 0x000E

	flagSync = 0x0001

	videoHeaderSize = 32
	multiHeaderSize = 16
)

var errVideoBlock = errors.New("milestone: legacy video block data is not supported")

// frame is one coded picture in decode order. Times are recorder UTC milliseconds.
type frame struct {
	codec uint16
	key   bool
	sync  int64 // time of the GOP's keyframe
	time  int64 // arrival time at the recorder, in decode order
	data  []byte
}

// parseFrames extracts video pictures from one response part. A part is a
// GenericByteData packet, a multi-packet wrapper, or a bare JPEG.
func parseFrames(b []byte, frames []frame) ([]frame, error) {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xD8 {
		return append(frames, frame{codec: codecJPEG, key: true, data: b}), nil
	}
	for len(b) > 0 {
		if len(b) < 6 {
			return frames, errDesync
		}
		dataType := binary.BigEndian.Uint16(b)
		switch {
		case dataType == gbdMultiPacket:
			// Wraps one video and one audio packet; the inner packets follow.
			if len(b) < multiHeaderSize {
				return frames, errDesync
			}
			b = b[multiHeaderSize:]
			continue
		case dataType >= 0x0001 && dataType <= 0x000F:
			return frames, errVideoBlock
		}

		size := int(binary.BigEndian.Uint32(b[2:]))
		if size > len(b) {
			return frames, errDesync
		}
		switch dataType {
		case gbdVideoPacket:
			if size < videoHeaderSize {
				return frames, errDesync
			}
			frames = append(frames, frame{
				codec: binary.BigEndian.Uint16(b[6:]),
				key:   binary.BigEndian.Uint16(b[10:])&flagSync != 0,
				sync:  int64(binary.BigEndian.Uint64(b[12:])),
				time:  int64(binary.BigEndian.Uint64(b[20:])),
				data:  b[videoHeaderSize:size],
			})
		case gbdAudioPacket, gbdMetadata:
			// Audio is a separate device in XProtect; metadata is not video.
		default:
			return frames, fmt.Errorf("milestone: unknown GenericByteData type 0x%04X", dataType)
		}
		if size == 0 {
			return frames, errDesync
		}
		b = b[size:]
	}
	return frames, nil
}

func codecName(id uint16) string {
	switch id {
	case codecJPEG:
		return "JPEG"
	case codecH264:
		return "H.264"
	case codecH265:
		return "H.265"
	case 0x0004, 0x0005, 0x0006, 0x000B:
		return "MPEG-4"
	case 0x0007, 0x0009:
		return "H.263"
	case 0x000C:
		return "VC-1"
	case 0x000F:
		return "AV1"
	case 0x0080:
		return "MxPEG"
	}
	return fmt.Sprintf("codec 0x%04X", id)
}
