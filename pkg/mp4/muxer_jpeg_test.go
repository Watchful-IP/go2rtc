package mp4

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestJPEGKeepsSourceTimeline(t *testing.T) {
	m := &Muxer{}
	m.AddTrack(&core.Codec{Name: core.CodecJPEG, ClockRate: 90000})
	_, err := m.GetInit()
	require.NoError(t, err)

	dem := &Demuxer{tracks: map[uint32]*track{1: {codec: &core.Codec{Name: core.CodecJPEG, ClockRate: 90000}, scale: 90000, description: 1}}}
	demux := func(ts uint32) Sample {
		data := m.GetPayload(0, &rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
		require.NotNil(t, data)
		samples, err := dem.Demux(data)
		require.NoError(t, err)
		require.Len(t, samples, 1)
		return samples[0]
	}

	// Start just before the 90 kHz clock wraps, then stall for longer than the one-second
	// clamp the generic path applies for Safari.
	start := uint32(1<<32 - 1000)
	first := demux(start)
	second := demux(start + 3600)
	stalled := demux(start + 3600 + 2*90000)

	require.Equal(t, []uint64{0, 3600, 3600 + 2*90000}, []uint64{first.DecodeTime, second.DecodeTime, stalled.DecodeTime})
	require.Equal(t, []uint32{3600, 3600, 2 * 90000}, []uint32{first.Duration, second.Duration, stalled.Duration})

	// A frame stamped before its predecessor cannot be placed on the timeline.
	require.Nil(t, m.GetPayload(0, &rtp.Packet{Header: rtp.Header{Timestamp: start}, Payload: []byte{0xFF, 0xD8, 0xFF, 0xD9}}))
}
