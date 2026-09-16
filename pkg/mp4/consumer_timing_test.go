package mp4

import (
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPendingAudioIsBoundedAndOwnsPayload(t *testing.T) {
	consumer := NewConsumer(nil)
	defer consumer.Stop()
	payload := make([]byte, 1024, 64<<20)
	p := &rtp.Packet{Payload: payload}
	core.SetSampleTiming(p, core.SampleTiming{Duration: 1024})
	for i := 0; i < 1000; i++ {
		consumer.writePacket(0, p, false, false)
	}
	require.LessOrEqual(t, len(consumer.pending), 128)
	require.LessOrEqual(t, consumer.pendingBytes, 64<<10)
	payload[0] = 1
	for _, sample := range consumer.pending {
		require.Equal(t, byte(0), sample.packet.Payload[0])
		require.Less(t, cap(sample.packet.Payload), 2048)
	}
}

func TestReconnectWaitsForNewKeyframe(t *testing.T) {
	c := NewConsumer(nil)
	defer c.Stop()
	c.hasVideo = true
	c.muxer.AddTrack(&core.Codec{Name: core.CodecH264, ClockRate: 90000})
	packet := func(id uint32, ts uint64) *rtp.Packet {
		p := &rtp.Packet{Payload: []byte{0, 0, 0, 2, 0x65, 0x80}}
		core.SetSampleTiming(p, core.SampleTiming{ClockID: id, DecodeTime: ts, Duration: 9000})
		return p
	}
	c.writePacket(0, packet(1, 0), true, true)
	initial := c.Send
	require.Positive(t, initial)
	c.writePacket(0, packet(2, 0), true, false)
	require.Equal(t, initial, c.Send)
	require.False(t, c.start)
	c.writePacket(0, packet(1, 9000), true, true)
	require.Equal(t, initial, c.Send)
	c.writePacket(0, packet(2, 9000), true, true)
	require.Greater(t, c.Send, initial)
	require.True(t, c.start)
}
