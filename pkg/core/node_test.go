package core

import (
	"sync"
	"testing"
)

// Run with -race: removing a consumer while packets are forwarded must not touch the
// slice the forwarding loop is reading.
func TestRemovingAChildWhilePacketsFlow(t *testing.T) {
	receiver := NewReceiver(nil, &Codec{Name: CodecH264})
	children := make([]*Node, 8)
	for i := range children {
		children[i] = &Node{Input: func(*Packet) {}}
		receiver.AppendChild(children[i])
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			receiver.Input(&Packet{})
		}
	}()
	for i, child := range children {
		receiver.RemoveChild(child)
		receiver.AppendChild(children[(i+1)%len(children)])
	}
	wg.Wait()
}
