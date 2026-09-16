package h264

import (
	"bytes"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRepairAVCCDoesNotMutateSharedPacket(t *testing.T) {
	original := []byte{0, 0, 0, 2, 0x06, 0x80, 0, 0, 0, 2, 0x65, 0x80}
	p := &rtp.Packet{Payload: bytes.Clone(original)}
	handler := RepairAVCC(&core.Codec{}, func(out *rtp.Packet) { require.Equal(t, original[6:], out.Payload) })
	handler(p)
	handler(p)
	require.Equal(t, original, p.Payload)
}
func TestRepairAVCCDropsEmptySEI(t *testing.T) {
	handler := RepairAVCC(&core.Codec{}, func(*rtp.Packet) { t.Fatal("invalid packet forwarded") })
	for _, payload := range [][]byte{nil, {0}, {0, 0, 0, 1, 0x06}, {0xff, 0xff, 0xff, 0xff, 0x06}} {
		handler(&rtp.Packet{Payload: payload})
	}
}
