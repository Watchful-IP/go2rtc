package mp4

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// jpegTrack feeds frames stamped on the source clock while the muxer's own clock
// advances by waited, and returns each frame's tfdt and duration.
type jpegTrack struct {
	t     *testing.T
	m     *Muxer
	dem   *Demuxer
	now   time.Duration
	stamp uint32
}

func newJPEGTrack(t *testing.T, start uint32) *jpegTrack {
	j := &jpegTrack{t: t, m: &Muxer{}, stamp: start}
	j.m.now = func() time.Duration { return j.now }
	j.m.AddTrack(&core.Codec{Name: core.CodecJPEG, ClockRate: 90000})
	_, err := j.m.GetInit()
	require.NoError(t, err)
	j.dem = &Demuxer{tracks: map[uint32]*track{1: {codec: &core.Codec{Name: core.CodecJPEG, ClockRate: 90000}, scale: 90000, description: 1}}}
	return j
}

func (j *jpegTrack) frame(stampDelta uint32, waited time.Duration) Sample {
	j.stamp += stampDelta
	j.now += waited
	data := j.m.GetPayload(0, &rtp.Packet{Header: rtp.Header{Timestamp: j.stamp}, Payload: []byte{0xFF, 0xD8, 0xFF, 0xD9}})
	require.NotNil(j.t, data)
	samples, err := j.dem.Demux(data)
	require.NoError(j.t, err)
	require.Len(j.t, samples, 1)
	return samples[0]
}

const frame = 3600 // 40 ms at 90 kHz

func TestJPEGKeepsSourceTimeline(t *testing.T) {
	// Start just before the 90 kHz clock wraps.
	j := newJPEGTrack(t, 1<<32-1000)
	first := j.frame(0, 0)
	second := j.frame(frame, 40*time.Millisecond)
	// Queued frames drain in a burst: the source clock, not the muxer's, sets the gap.
	burst := j.frame(frame, 0)
	require.Equal(t, []uint64{0, frame, 2 * frame}, []uint64{first.DecodeTime, second.DecodeTime, burst.DecodeTime})

	// A stall moves the next frame's start but not its duration, or it would overlap
	// the frames after it.
	short := j.frame(81000, 900*time.Millisecond)
	long := j.frame(2*90000, 2*time.Second)
	require.Equal(t, uint64(2*frame+81000), short.DecodeTime)
	require.Equal(t, uint64(2*frame+81000+2*90000), long.DecodeTime)
	require.Equal(t, []uint32{frame, frame, frame, frame, frame}, []uint32{first.Duration, second.Duration, burst.Duration, short.Duration, long.Duration})
}

func TestJPEGKeepsOutagesLongerThanHalfTheClock(t *testing.T) {
	j := newJPEGTrack(t, 1000000)
	j.frame(0, 0)
	outage := 7 * time.Hour
	after := j.frame(uint32(outage/time.Second)*90000, outage)
	require.Equal(t, uint64(outage/time.Second)*90000, after.DecodeTime)
}

func TestJPEGContinuesAcrossSourceClockResets(t *testing.T) {
	j := newJPEGTrack(t, 5000)
	j.frame(0, 0)
	// An RTP source reconnects with a new random clock base, then keeps its cadence.
	reset := j.frame(3_000_000_000, 40*time.Millisecond)
	next := j.frame(frame, 40*time.Millisecond)
	// A wall clock steps back a second.
	stepped := j.frame(^uint32(90000-1), 40*time.Millisecond)
	require.Equal(t, []uint64{frame, 2 * frame, 3 * frame}, []uint64{reset.DecodeTime, next.DecodeTime, stepped.DecodeTime})
}

func TestJPEGAcceptsRTPSources(t *testing.T) {
	codec := &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: 26}
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
	cons := NewConsumer([]*core.Media{{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecJPEG}}}})
	require.NoError(t, cons.AddTrack(media, codec, core.NewReceiver(media, codec)))
}

func TestJPEGContentType(t *testing.T) {
	require.Equal(t, `video/mp4; codecs="jpeg"`, ContentType([]*core.Codec{{Name: core.CodecJPEG}}))
}
