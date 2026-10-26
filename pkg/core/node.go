package core

import (
	"sync"

	"github.com/pion/rtp"
)

//type Packet struct {
//	Payload     []byte
//	Timestamp   uint32 // PTS if DTS == 0 else DTS
//	Composition uint32 // CTS = PTS-DTS (for support B-frames)
//	Sequence    uint16
//}

type Packet = rtp.Packet

// HandlerFunc - process input packets (just like http.HandlerFunc)
type HandlerFunc func(packet *Packet)

// Filter - a decorator for any HandlerFunc
type Filter func(handler HandlerFunc) HandlerFunc

// Node - Receiver or Sender or Filter (transform)
type Node struct {
	Codec  *Codec
	Input  HandlerFunc
	Output HandlerFunc

	id     uint32
	childs []*Node
	parent *Node

	mu sync.Mutex
	// Watchful: parent changes when consumers move between connections while they close.
	// Not mu: a lossless Sender holds mu while blocked on a full buffer.
	parentMu sync.Mutex
}

func (n *Node) setParent(parent *Node) {
	n.parentMu.Lock()
	n.parent = parent
	n.parentMu.Unlock()
}

func (n *Node) getParent() *Node {
	n.parentMu.Lock()
	defer n.parentMu.Unlock()
	return n.parent
}

func (n *Node) WithParent(parent *Node) *Node {
	parent.AppendChild(n)
	return n
}

func (n *Node) AppendChild(child *Node) {
	n.mu.Lock()
	n.childs = append(n.childs, child)
	n.mu.Unlock()

	child.setParent(n)
}

func (n *Node) RemoveChild(child *Node) {
	n.mu.Lock()
	for i, ch := range n.childs {
		if ch == child {
			// Watchful: build a new slice rather than shift in place, so a packet loop still
			// ranging over the old one never sees its elements move.
			childs := make([]*Node, 0, len(n.childs)-1)
			n.childs = append(append(childs, n.childs[:i]...), n.childs[i+1:]...)
			break
		}
	}
	n.mu.Unlock()
}

func (n *Node) Close() {
	if parent := n.getParent(); parent != nil {
		parent.RemoveChild(n)

		parent.mu.Lock()
		empty := len(parent.childs) == 0
		parent.mu.Unlock()
		if empty {
			parent.Close()
		}
	} else {
		n.mu.Lock()
		childs := n.childs
		n.mu.Unlock()
		for _, child := range childs {
			child.Close()
		}
	}
}

func MoveNode(dst, src *Node) {
	src.mu.Lock()
	childs := src.childs
	src.childs = nil
	src.mu.Unlock()

	dst.mu.Lock()
	dst.childs = childs
	dst.mu.Unlock()

	for _, child := range childs {
		child.setParent(dst)
	}
}
