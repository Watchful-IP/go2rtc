package h265

import (
	"bytes"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestRTPDepayDiscardsMarkedPictureBeforeGap(t *testing.T) {
	for _, fragmented := range []bool{false, true} {
		t.Run(map[bool]string{false: "single NAL", true: "fragmented NAL"}[fragmented], func(t *testing.T) {
			var got [][]byte
			depay := RTPDepay(&core.Codec{H265ConservativeRecovery: true}, func(p *rtp.Packet) { got = append(got, bytes.Clone(p.Payload)) })
			if fragmented {
				depay(lossPacket(10, 100, false, []byte{98, 1, 129, 0x80, 7}))
				depay(lossPacket(11, 100, true, []byte{98, 1, 65, 8}))
			} else {
				depay(lossPacket(11, 100, true, []byte{2, 1, 0x80, 7, 8}))
			}
			require.Empty(t, got)
			// Captured Uniview pictures had intact FU/marker flags but failed VideoToolbox;
			// the following packet revealed the loss, after the picture was already sent.
			depay(lossPacket(20, 200, true, []byte{2, 1, 0x80, 9}))
			depay(lossPacket(21, 300, true, []byte{2, 1, 0x80, 10}))
			require.Empty(t, got)
			key := []byte{38, 1, 0x80, 11}
			depay(lossPacket(22, 400, true, key))
			require.Equal(t, [][]byte{lossNALs(key)}, got)
			frame := []byte{2, 1, 0x80, 12}
			depay(lossPacket(23, 500, true, frame))
			require.Len(t, got, 1)
			depay(lossPacket(24, 600, false, []byte{64, 1, 7}))
			require.Equal(t, [][]byte{lossNALs(key), lossNALs(frame)}, got)
		})
	}
}

func TestRTPDepayLookaheadPreservesHeaderAndRollover(t *testing.T) {
	var got []*rtp.Packet
	depay := RTPDepay(&core.Codec{H265ConservativeRecovery: true}, func(p *rtp.Packet) { q := *p; q.Payload = bytes.Clone(p.Payload); got = append(got, &q) })
	p := lossPacket(65535, 123, true, []byte{2, 1, 0x80, 7})
	p.SSRC = 42
	depay(p)
	require.Empty(t, got)
	depay(lossPacket(0, 456, true, []byte{2, 1, 0x80, 8}))
	require.Len(t, got, 1)
	require.Equal(t, uint16(65535), got[0].SequenceNumber)
	require.Equal(t, uint32(123), got[0].Timestamp)
	require.Equal(t, uint32(42), got[0].SSRC)
	require.True(t, got[0].Marker)
	require.Equal(t, lossNALs([]byte{2, 1, 0x80, 7}), got[0].Payload)
	// Flush the second buffer and exercise reuse of the first.
	depay(lossPacket(1, 789, true, []byte{2, 1, 0x80, 9}))
	depay(lossPacket(2, 900, false, []byte{64, 1, 7}))
	require.Len(t, got, 3)
	require.Equal(t, lossNALs([]byte{2, 1, 0x80, 8}), got[1].Payload)
	require.Equal(t, lossNALs([]byte{2, 1, 0x80, 9}), got[2].Payload)
}

func TestRTPDepayDefaultHasNoLookahead(t *testing.T) {
	var normal, conservative int
	depay := RTPDepay(&core.Codec{}, func(*rtp.Packet) { normal++ })
	delayed := RTPDepay(&core.Codec{H265ConservativeRecovery: true}, func(*rtp.Packet) { conservative++ })
	for i := uint16(1); i <= 3; i++ {
		p := lossPacket(i, uint32(i)*3000, true, []byte{2, 1, 0x80, byte(i)})
		depay(p)
		delayed(p)
		require.Equal(t, int(i), normal)
		require.Equal(t, int(i)-1, conservative)
	}
}
