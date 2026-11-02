package core

import (
	"github.com/stretchr/testify/require"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestPictureQueueByteAndAgeLimits(t *testing.T) {
	for _, limit := range []string{"bytes", "enqueue age", "dequeue age"} {
		t.Run(limit, func(t *testing.T) {
			codec := &Codec{Name: CodecH265, PayloadType: PayloadTypeRAW}
			r := NewReceiver(nil, codec)
			rtpCodec := codec.Clone()
			rtpCodec.PayloadType = 96
			s := NewSender(nil, rtpCodec).WithParent(r)
			defer s.Close()
			var ns atomic.Int64
			ns.Store(time.Now().UnixNano())
			s.queueNow = func() time.Time { return time.Unix(0, ns.Load()) }
			key := &Packet{Payload: []byte{0, 0, 0, 2, 0x26, 1}}
			p := &Packet{Payload: []byte{0, 0, 0, 2, 2, 1}}
			s.Input(key)
			switch limit {
			case "bytes":
				huge := &Packet{Payload: make([]byte, pictureQueueMaxBytes+1)}
				s.Input(huge)
			case "enqueue age":
				ns.Add(int64(pictureQueueMaxAge + 1))
				s.Input(p)
			case "dequeue age":
				ns.Add(int64(pictureQueueMaxAge + 1))
				received := make(chan struct{}, 1)
				s.Handler = func(*Packet) { received <- struct{}{} }
				s.Start()
				deadline := time.Now().Add(time.Second)
				for {
					s.mu.Lock()
					dropped := s.Drops
					s.mu.Unlock()
					if dropped > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("stale dequeue not discarded")
					}
					runtime.Gosched()
				}
				s.Input(key)
				select {
				case <-received:
				case <-time.After(time.Second):
					t.Fatal("fresh keyframe not delivered")
				}
				s.Close()
				s.Wait()
				require.Positive(t, s.Drops)
				return
			}
			require.Empty(t, s.buf)
			require.True(t, s.waitKey)
			s.Input(p)
			require.Empty(t, s.buf)
			s.Input(key)
			require.Len(t, s.buf, 1)
		})
	}
}
func craSender(t *testing.T) *Sender {
	codec := &Codec{Name: CodecH265, PayloadType: PayloadTypeRAW}
	rtpCodec := codec.Clone()
	rtpCodec.PayloadType = 96
	s := NewSender(nil, rtpCodec).WithParent(NewReceiver(nil, codec))
	t.Cleanup(s.Close)
	return s
}

func TestPictureQueueStartsOnCRAAndDropsItsLeadingPictures(t *testing.T) {
	s := craSender(t)
	cra := &Packet{Payload: []byte{0, 0, 0, 2, 21 << 1, 1}}
	rasl := &Packet{Payload: []byte{0, 0, 0, 2, 9 << 1, 1}}
	trail := &Packet{Payload: []byte{0, 0, 0, 2, 1 << 1, 1}}
	s.Input(cra)
	s.Input(rasl)
	s.Input(trail)
	s.Input(rasl) // after a trailing picture, RASL of a later CRA is decodable
	require.Len(t, s.buf, 3)
	require.Equal(t, 1, s.Drops)
}

func TestPictureQueueAgeIgnoresAnAlreadyDequeuedPicture(t *testing.T) {
	s := craSender(t)
	start := time.Now()
	now := start
	s.queueNow = func() time.Time { return now }
	key := &Packet{Payload: []byte{0, 0, 0, 2, 0x26, 1}}
	p := &Packet{Payload: []byte{0, 0, 0, 2, 2, 1}}
	s.Input(key)
	now = start.Add(1800 * time.Millisecond)
	s.Input(p)
	// The consumer takes the keyframe and is still writing it.
	s.mu.Lock()
	s.dequeued(<-s.buf)
	s.mu.Unlock()
	now = start.Add(2100 * time.Millisecond)
	s.Input(p)
	require.Len(t, s.buf, 2, "the queued picture has waited only 300 ms")
	require.Zero(t, s.Drops)
}
