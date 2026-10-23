package h265

import (
	"bytes"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

// Captured from a Uniview NVR16 (2026-10-23): 407 bytes, reserved NAL type 10,
// marker set, on H.264 and H.265 tracks alike.
func univiewVendorPayload() []byte {
	b := make([]byte, 407)
	copy(b, []byte{0x15, 0x01, 0x94, 0x00, 0x16, 0x00, 0x12, 0x8c, 0x01, 0x01, 0x8c})
	return b
}

func TestRTPDepayVendorPacketSequence(t *testing.T) {
	key := lossNALs([]byte{38, 1, 10, 11})
	frame := func(n byte) []byte { return lossNALs([]byte{2, 1, n}) }
	tests := []struct {
		name    string
		packets []*rtp.Packet
		want    [][]byte
	}{
		{"next packet reuses the vendor sequence number", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 147, 10}),
			lossPacket(11, 100, true, []byte{98, 1, 83, 11}),
			lossPacket(12, 4000, true, univiewVendorPayload()),
			lossPacket(12, 4500, true, []byte{2, 1, 1}),
			lossPacket(13, 8500, true, univiewVendorPayload()),
			lossPacket(13, 9000, true, []byte{2, 1, 2}),
			lossPacket(14, 13500, true, []byte{2, 1, 3}),
		}, [][]byte{key, frame(1), frame(2), frame(3)}},
		{"losing the packet after a vendor packet still waits for a keyframe", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 147, 10}),
			lossPacket(11, 100, true, []byte{98, 1, 83, 11}),
			lossPacket(12, 4000, true, univiewVendorPayload()),
			lossPacket(13, 9000, true, []byte{2, 1, 2}),
			lossPacket(14, 13500, true, []byte{2, 1, 3}),
		}, [][]byte{key}},
		{"vendor packet inside a fragmented picture", []*rtp.Packet{
			lossPacket(10, 100, false, []byte{98, 1, 147, 10}),
			lossPacket(11, 100, false, univiewVendorPayload()),
			lossPacket(11, 100, true, []byte{98, 1, 83, 11}),
		}, [][]byte{key}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got [][]byte
			depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
			for _, p := range tt.packets {
				depay(p)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d access units %x, want %d", len(got), got, len(tt.want))
			}
			for i := range tt.want {
				if !bytes.Equal(got[i], tt.want[i]) {
					t.Fatalf("access unit %d: got %x, want %x", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRTPDepayVendorPacketWithConservativeRecovery(t *testing.T) {
	var got int
	depay := RTPDepay(&core.Codec{H265ConservativeRecovery: true}, func(*rtp.Packet) { got++ })
	depay(lossPacket(10, 100, false, []byte{98, 1, 147, 10}))
	depay(lossPacket(11, 100, true, []byte{98, 1, 83, 11}))
	depay(lossPacket(12, 4500, true, []byte{2, 1, 1}))
	depay(lossPacket(13, 8500, true, univiewVendorPayload()))
	depay(lossPacket(13, 9000, true, []byte{2, 1, 2}))
	depay(lossPacket(14, 13500, true, []byte{2, 1, 3}))
	// The last non-keyframe stays pending until the next packet.
	if got != 3 {
		t.Fatalf("got %d access units, want 3", got)
	}
}
