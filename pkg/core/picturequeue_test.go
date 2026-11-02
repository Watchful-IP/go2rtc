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
func TestPictureQueueDoesNotTreatCRAAsIndependent(t *testing.T) {
	require.False(t, randomAccessPicture(CodecH265, []byte{0, 0, 0, 2, 42, 1}))
	require.True(t, randomAccessPicture(CodecH265, []byte{0, 0, 0, 2, 38, 1}))
}
