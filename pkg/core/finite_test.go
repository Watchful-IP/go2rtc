package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLosslessSenderBackpressure(t *testing.T) {
	codec := &Codec{Name: CodecH264, PayloadType: PayloadTypeRAW}
	r := NewReceiver(nil, codec)
	r.Lossless = true
	s := NewSender(nil, codec)
	s.WithParent(r)
	// Fill the bounded queue before starting the reader.
	for i := 0; i < cap(s.buf); i++ {
		r.WriteRTP(&Packet{})
	}
	blocked := make(chan struct{})
	go func() { s.Input(&Packet{}); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("lossless sender dropped instead of blocking")
	case <-time.After(20 * time.Millisecond):
	}
	s.Close()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("Stop did not unblock sender")
	}
	require.Zero(t, s.Drops)
}

func TestLosslessSenderProducerCancellation(t *testing.T) {
	codec := &Codec{Name: CodecH264, PayloadType: PayloadTypeRAW}
	r := NewReceiver(nil, codec)
	r.Lossless = true
	s := NewSender(nil, codec).WithParent(r)
	defer s.Close()
	for i := 0; i < cap(s.buf); i++ {
		s.Input(&Packet{})
	}
	done := make(chan struct{})
	go func() { s.Input(&Packet{}); close(done) }()
	r.End(nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer cancellation did not unblock sender")
	}
	require.Zero(t, s.Drops)
}
