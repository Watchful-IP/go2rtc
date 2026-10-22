package exacq

import (
	"encoding/binary"
	"fmt"
	"time"
)

const streamHeaderSize = 32

// Media packet kinds are independent of the stream's codec standard.
const (
	MediaAudio uint8 = iota
	MediaIFrame
	MediaPFrame
	MediaBFrame
	MediaSerial
	MediaSubtitle
)

// MediaPacket is the decoded payload of a LIVE or STORED STREAM packet.
// StreamID is a server stream identifier, not necessarily a camera CASID.
// Type is the packet kind above, not a codec identifier. Flags retain their wire
// value without assuming undocumented bit semantics. Motion and Data alias the
// packet payload.
type MediaPacket struct {
	StreamID     uint32
	Seconds      uint32
	Milliseconds uint16
	Flags        uint8
	Type         uint8
	Reserved     [16]byte
	Motion       []byte
	Data         []byte
}

// Timestamp returns the recorder timestamp. An untimed cached frame has zero
// seconds and returns the zero time; it must not establish the playback clock.
// The protocol carries no separate decode timestamp in this header.
func (p *MediaPacket) Timestamp() time.Time {
	if p.Seconds == 0 {
		return time.Time{}
	}
	return time.Unix(int64(p.Seconds), int64(p.Milliseconds)*int64(time.Millisecond)).UTC()
}

// parseMediaPacket implements the wire tagStreamHeader layout established by
// libevAPI DWARF and CConnectionImpl::Response. The callback tagPayloadType has
// a different layout and must never be decoded with this function.
func parseMediaPacket(payload []byte) (*MediaPacket, error) {
	if len(payload) < streamHeaderSize {
		return nil, fmt.Errorf("exacq: media header truncated: %d bytes", len(payload))
	}
	milliseconds := binary.LittleEndian.Uint16(payload[8:10])
	if milliseconds > 999 {
		return nil, fmt.Errorf("exacq: invalid media millisecond fraction: %d", milliseconds)
	}
	motionLength := binary.LittleEndian.Uint32(payload[12:16])
	// Compare before converting to int, including on 32-bit builds.
	if uint64(motionLength) > uint64(len(payload)-streamHeaderSize) {
		return nil, fmt.Errorf("exacq: motion data truncated: %d bytes declared, %d available", motionLength, len(payload)-streamHeaderSize)
	}
	mediaOffset := streamHeaderSize + int(motionLength)
	p := &MediaPacket{
		StreamID:     binary.LittleEndian.Uint32(payload[0:4]),
		Seconds:      binary.LittleEndian.Uint32(payload[4:8]),
		Milliseconds: milliseconds,
		Flags:        payload[10],
		Type:         payload[11],
		Motion:       payload[streamHeaderSize:mediaOffset],
		Data:         payload[mediaOffset:],
	}
	copy(p.Reserved[:], payload[16:32])
	return p, nil
}
