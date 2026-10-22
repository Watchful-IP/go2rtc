package streams

import (
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// scriptedSource is an RTSP-like H.264 producer the test feeds packet by packet.
type scriptedSource struct {
	core.Connection
	feed    chan *core.Packet
	started chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func newScriptedSource() *scriptedSource {
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
	return &scriptedSource{
		Connection: core.Connection{Medias: []*core.Media{{
			Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec},
		}}},
		feed:    make(chan *core.Packet, 64),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (s *scriptedSource) Start() error {
	close(s.started)
	for {
		select {
		case packet := <-s.feed:
			for _, receiver := range s.Receivers {
				receiver.Input(packet)
			}
		case <-s.stopped:
			return nil
		}
	}
}

func (s *scriptedSource) Stop() error {
	s.once.Do(func() { close(s.stopped) })
	return nil
}

func (s *scriptedSource) isStopped() bool {
	select {
	case <-s.stopped:
		return true
	default:
		return false
	}
}

// send delivers packets and waits until the source has handed them to its receivers.
func (s *scriptedSource) send(packets ...*core.Packet) {
	for _, packet := range packets {
		s.feed <- packet
	}
	for len(s.feed) > 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
}

func pkt(source uint32, seq uint16, marker bool, payload ...byte) *core.Packet {
	return &rtp.Packet{Header: rtp.Header{SSRC: source, SequenceNumber: seq, Marker: marker}, Payload: payload}
}

var (
	pFrame  = []byte{0x41, 0x9a}
	sps     = []byte{0x67, 0x64}
	idrHead = []byte{0x7c, 0x85} // FU-A start of an IDR
	idrTail = []byte{0x7c, 0x45} // FU-A end of an IDR
)

// recorder is a consumer that keeps every packet it is sent, in order.
type recorder struct {
	mu      sync.Mutex
	got     []*core.Packet
	senders []*core.Sender
}

func (r *recorder) GetMedias() []*core.Media {
	return []*core.Media{{Kind: core.KindVideo, Direction: core.DirectionSendonly,
		Codecs: []*core.Codec{{Name: core.CodecH264}}}}
}

func (r *recorder) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, track.Codec)
	sender.Handler = func(packet *core.Packet) {
		r.mu.Lock()
		r.got = append(r.got, packet)
		r.mu.Unlock()
	}
	sender.HandleRTP(track)
	r.senders = append(r.senders, sender)
	return nil
}

func (r *recorder) Stop() error {
	for _, sender := range r.senders {
		sender.Close()
	}
	return nil
}

func (r *recorder) trace() []string {
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, packet := range r.got {
		name := "a"
		if packet.SSRC == 2 {
			name = "b"
		}
		out = append(out, name+string(rune('0'+packet.SequenceNumber)))
	}
	return out
}

func swapRig(t *testing.T) (*Stream, *recorder, *scriptedSource, *scriptedSource) {
	a, b := newScriptedSource(), newScriptedSource()
	HandleFunc("scripted", func(url string) (core.Producer, error) {
		if url == "scripted:b" {
			return b, nil
		}
		return a, nil
	})
	t.Cleanup(func() { delete(handlers, "scripted") })

	stream := NewStream("scripted:a")
	cons := &recorder{}
	require.NoError(t, stream.AddConsumer(cons))
	t.Cleanup(func() { stream.RemoveConsumer(cons) })
	return stream, cons, a, b
}

func TestSwapMovesConsumersOnTheNewKeyframeAfterTheOldFrameEnds(t *testing.T) {
	stream, cons, a, b := swapRig(t)
	a.send(pkt(1, 1, true, pFrame...))

	swapped := make(chan error, 1)
	go func() { swapped <- stream.Swap("scripted:b", 2*time.Second) }()
	<-b.started

	b.send(pkt(2, 1, true, pFrame...))                             // before any keyframe: nobody should see it
	a.send(pkt(1, 2, false, pFrame...))                            // old picture still open
	b.send(pkt(2, 2, false, sps...), pkt(2, 3, false, idrHead...)) // held
	a.send(pkt(1, 3, true, pFrame...))                             // old picture ends: consumers move
	b.send(pkt(2, 4, true, idrTail...), pkt(2, 5, true, pFrame...))

	require.NoError(t, <-swapped)
	require.Equal(t, []string{"a1", "a2", "a3", "b2", "b3", "b4", "b5"}, cons.trace())
	require.True(t, a.isStopped())
	require.False(t, b.isStopped())
	require.Equal(t, "scripted:b", stream.producers[0].url)
}

func TestSwapWithoutAKeyframeKeepsTheOldSource(t *testing.T) {
	stream, cons, a, b := swapRig(t)
	a.send(pkt(1, 1, true, pFrame...))

	swapped := make(chan error, 1)
	go func() { swapped <- stream.Swap("scripted:b", 100*time.Millisecond) }()
	<-b.started
	b.send(pkt(2, 1, true, pFrame...))

	require.ErrorIs(t, <-swapped, ErrSwapNoKeyframe)
	a.send(pkt(1, 2, true, pFrame...))
	require.Equal(t, []string{"a1", "a2"}, cons.trace())
	require.True(t, b.isStopped())
	require.False(t, a.isStopped())
	require.Equal(t, "scripted:a", stream.producers[0].url)
}

func TestSwapMovesAtOnceWhenTheOldSourceHasGoneSilent(t *testing.T) {
	stream, cons, a, b := swapRig(t)
	a.send(pkt(1, 1, false, pFrame...)) // the old source dies mid-picture

	swapped := make(chan error, 1)
	go func() { swapped <- stream.Swap("scripted:b", 2*time.Second) }()
	<-b.started
	b.send(pkt(2, 1, false, sps...))
	time.Sleep(holdForBoundary + 20*time.Millisecond)
	b.send(pkt(2, 2, true, idrTail...))

	require.NoError(t, <-swapped)
	require.Equal(t, []string{"a1", "b1", "b2"}, cons.trace())
}

func TestSwapOfAnIdleStreamOnlyChangesTheNextDial(t *testing.T) {
	stream := NewStream("scripted:a")
	require.NoError(t, stream.Swap("scripted:b", time.Second))
	require.Equal(t, "scripted:b", stream.producers[0].url)
}

func TestStartsKeyframe(t *testing.T) {
	h264 := &core.Codec{Name: core.CodecH264, PayloadType: 96}
	h265 := &core.Codec{Name: core.CodecH265, PayloadType: 96}
	for _, tc := range []struct {
		codec   *core.Codec
		payload []byte
		want    bool
	}{
		{h264, sps, true},
		{h264, []byte{0x65, 0x88}, true},       // single IDR
		{h264, idrHead, true},                  // FU-A start of an IDR
		{h264, idrTail, false},                 // FU-A continuation
		{h264, []byte{0x7c, 0x81}, false},      // FU-A start of a P slice
		{h264, []byte{0x78, 0, 2, 0x67}, true}, // STAP-A led by SPS
		{h264, pFrame, false},
		{h265, []byte{0x40, 0x01}, true},             // VPS
		{h265, []byte{0x26, 0x01}, true},             // IDR_W_RADL
		{h265, []byte{0x62, 0x01, 0x93}, true},       // FU start of IDR_W_RADL
		{h265, []byte{0x62, 0x01, 0x01}, false},      // FU continuation
		{h265, []byte{0x02, 0x01}, false},            // TRAIL_R
		{h265, []byte{0x60, 0x01, 0, 2, 0x40}, true}, // AP led by VPS
	} {
		require.Equal(t, tc.want, startsKeyframe(tc.codec, &rtp.Packet{Payload: tc.payload}), "%x", tc.payload)
	}
}
