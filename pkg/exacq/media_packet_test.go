package exacq

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestParseMediaPacket(t *testing.T) {
	// Synthetic fixture follows tagStreamHeader, not the SDK callback layout.
	payload := []byte{
		0x21, 0x43, 0x65, 0x87, // stream ID
		0x00, 0xf1, 0x53, 0x65, // recorder seconds = 1700000000
		0xe7, 0x03, 0x80, 0x01, // milliseconds, flags, I-frame kind
		0x03, 0x00, 0x00, 0x00, // motion length
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		0xaa, 0xbb, 0xcc, // motion metadata must not reach the video decoder
		0, 0, 0, 1, 0x67, 0x64, 0, 0x29,
	}
	p, err := parseMediaPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.StreamID != 0x87654321 || p.Seconds != 1700000000 || p.Milliseconds != 999 || p.Flags != 0x80 || p.Type != MediaIFrame {
		t.Fatalf("wrong header: %+v", p)
	}
	if got := p.Timestamp(); !got.Equal(time.Unix(1700000000, 999000000)) {
		t.Fatal(got)
	}
	if !bytes.Equal(p.Motion, []byte{0xaa, 0xbb, 0xcc}) || !bytes.Equal(p.Data, payload[35:]) || !bytes.Equal(p.Reserved[:], payload[16:32]) {
		t.Fatal("incorrect media boundary or reserved bytes")
	}
	// The parser preserves unknown types/flags and does not classify video by
	// bytes at offsets belonging to the SDK callback structure.
	payload[11] = 0xff
	p, err = parseMediaPacket(payload)
	if err != nil || p.Type != 0xff {
		t.Fatalf("unknown type lost: %v", err)
	}
}

func TestMediaPacketBounds(t *testing.T) {
	for n := 0; n < streamHeaderSize; n++ {
		if _, err := parseMediaPacket(make([]byte, n)); err == nil {
			t.Fatalf("accepted %d-byte header", n)
		}
	}
	for _, n := range []uint32{1, 32, 0x7fffffff, 0xffffffff} {
		payload := make([]byte, streamHeaderSize)
		binary.LittleEndian.PutUint32(payload[12:], n)
		if _, err := parseMediaPacket(payload); err == nil {
			t.Fatalf("accepted %d-byte missing motion data", n)
		}
	}
	for _, milliseconds := range []uint16{1000, 65535} {
		payload := make([]byte, streamHeaderSize)
		binary.LittleEndian.PutUint16(payload[8:], milliseconds)
		if _, err := parseMediaPacket(payload); err == nil {
			t.Fatalf("accepted %d millisecond fraction", milliseconds)
		}
	}
	p, err := parseMediaPacket(make([]byte, streamHeaderSize))
	if err != nil || !p.Timestamp().IsZero() || len(p.Data) != 0 {
		t.Fatalf("empty untimed packet: %+v %v", p, err)
	}
}

func FuzzParseMediaPacket(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, streamHeaderSize))
	f.Add(bytes.Repeat([]byte{0xff}, 64))
	f.Fuzz(func(t *testing.T, payload []byte) {
		p, err := parseMediaPacket(payload)
		if err != nil {
			return
		}
		if streamHeaderSize+len(p.Motion)+len(p.Data) != len(payload) {
			t.Fatal("payload boundaries do not match")
		}
		_ = p.Timestamp()
	})
}
