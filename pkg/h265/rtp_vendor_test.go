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

// Hikvision NVR playback (ISAPI/Streaming/tracks, "HIK Media Server V4.50.000")
// wraps every keyframe in empty RTP packets: two before the VPS and one after the
// IDR, each with the marker set and numbered from a separate counter. Counting them
// as loss held every following P-frame until the next keyframe, so recorded H.265
// played back one picture per GOP.
func TestRTPDepayHikvisionPlaybackEmptyPackets(t *testing.T) {
	vps, sps, pps := []byte{64, 1, 12}, []byte{66, 1, 1}, []byte{68, 1, 192}
	p := func(n byte) []byte { return []byte{2, 1, n} }
	empty := func(seq uint16, ts uint32) *rtp.Packet { return lossPacket(seq, ts, true, nil) }
	packets := []*rtp.Packet{
		lossPacket(276, 1000, true, p(1)),
		empty(3, 4000),
		empty(4, 4000),
		lossPacket(277, 4000, false, vps),
		lossPacket(278, 4000, false, sps),
		lossPacket(279, 4000, false, pps),
		lossPacket(280, 4000, false, []byte{98, 1, 147, 175}), // FU start, IDR_N_LP
		lossPacket(281, 4000, true, []byte{98, 1, 83, 176}),   // FU end
		empty(5, 4000),
		lossPacket(282, 10000, true, p(2)),
		lossPacket(283, 16000, true, p(3)),
	}
	var got [][]byte
	depay := RTPDepay(&core.Codec{}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
	for _, pkt := range packets {
		depay(pkt)
	}
	want := [][]byte{
		lossNALs(p(1)),
		lossNALs(vps, sps, pps, []byte{38, 1, 175, 176}),
		lossNALs(p(2)),
		lossNALs(p(3)),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d access units %x, want %d", len(got), got, len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("access unit %d: got %x, want %x", i, got[i], want[i])
		}
	}
}
