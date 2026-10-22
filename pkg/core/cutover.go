package core

import (
	"sync"
	"time"
)

// CutoverPair is a running track and its replacement from a new connection.
type CutoverPair struct {
	From, To *Receiver
}

// Cutover moves the consumers of a set of running tracks onto a new connection's
// tracks without a gap (Watchful). The new video is held back from its first
// keyframe until the old video finishes the access unit it is sending, then every
// pair moves at once and the held packets are delivered, so a consumer sees the end
// of one picture followed by the start of a decodable one.
type Cutover struct {
	pairs      []CutoverPair
	video      int
	isKeyframe func(*Packet) bool
	isBoundary func(*Packet) bool
	maxHold    time.Duration

	mu       sync.Mutex
	state    byte
	held     []*Packet
	heldAt   time.Time
	boundary bool
	gate     *Node
	observer *Node
	done     chan struct{}
}

const (
	cutoverWaiting byte = iota
	cutoverHolding
	cutoverMoved
	cutoverAborted
)

// maxHeldPackets bounds the held new video while the old one is mid-picture.
const maxHeldPackets = 4096

// NewCutover installs the cutover on the pairs. video is the index of the pair to
// align on keyframes, or -1 to move as soon as Start is called. maxHold bounds how
// long the new video waits for the old one to reach a frame boundary, which also
// covers an old connection that has gone silent.
func NewCutover(pairs []CutoverPair, video int, isKeyframe, isBoundary func(*Packet) bool, maxHold time.Duration) *Cutover {
	c := &Cutover{
		pairs: pairs, video: video, isKeyframe: isKeyframe, isBoundary: isBoundary, maxHold: maxHold,
		boundary: true, done: make(chan struct{}),
	}
	if video < 0 {
		return c
	}

	c.observer = &Node{id: NewID()}
	c.observer.Input = c.observeOld
	pairs[video].From.AppendChild(c.observer)

	c.gate = &Node{id: NewID()}
	c.gate.Input = c.gateNew
	pairs[video].To.AppendChild(c.gate)
	return c
}

// Start moves at once when there is no video to align on.
func (c *Cutover) Start() {
	if c.video >= 0 {
		return
	}
	c.mu.Lock()
	c.moveLocked()
	c.mu.Unlock()
}

// Done is closed once the consumers have moved or the cutover was aborted.
func (c *Cutover) Done() <-chan struct{} { return c.done }

// Moved reports whether the consumers are on the new tracks.
func (c *Cutover) Moved() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == cutoverMoved
}

// Abort leaves the consumers where they are. It returns false when they already moved.
func (c *Cutover) Abort() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == cutoverMoved {
		return false
	}
	if c.state != cutoverAborted {
		c.state = cutoverAborted
		c.detachLocked()
		close(c.done)
	}
	return true
}

func (c *Cutover) observeOld(packet *Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.boundary = c.isBoundary(packet)
	if c.state == cutoverHolding && (c.boundary || time.Since(c.heldAt) > c.maxHold) {
		c.moveLocked()
	}
}

func (c *Cutover) gateNew(packet *Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case cutoverWaiting:
		if !c.isKeyframe(packet) {
			return
		}
		c.state = cutoverHolding
		c.heldAt = time.Now()
		c.held = append(c.held, packet)
		if c.boundary {
			c.moveLocked()
		}
	case cutoverHolding:
		c.held = append(c.held, packet)
		if len(c.held) >= maxHeldPackets || time.Since(c.heldAt) > c.maxHold {
			c.moveLocked()
		}
	case cutoverMoved:
		// A packet already on its way to the gate when the consumers moved.
		c.pairs[c.video].To.forward(packet)
	}
}

func (c *Cutover) moveLocked() {
	c.detachLocked()
	for _, pair := range c.pairs {
		appendChildren(&pair.To.Node, &pair.From.Node)
	}
	if c.video >= 0 {
		to := c.pairs[c.video].To
		for _, packet := range c.held {
			to.forward(packet)
		}
	}
	c.held = nil
	c.state = cutoverMoved
	close(c.done)
}

func (c *Cutover) detachLocked() {
	if c.video < 0 {
		return
	}
	c.pairs[c.video].From.RemoveChild(c.observer)
	c.pairs[c.video].To.RemoveChild(c.gate)
}

// MoveLateConsumers moves consumers that bound to an old track after the cutover,
// before the old connection is stopped and would close them.
func (c *Cutover) MoveLateConsumers() {
	for _, pair := range c.pairs {
		appendChildren(&pair.To.Node, &pair.From.Node)
	}
}

func (r *Receiver) forward(packet *Packet) {
	r.mu.Lock()
	childs := r.childs
	r.mu.Unlock()
	for _, child := range childs {
		child.Input(packet)
	}
}

// MoveChildren moves src's children under dst, keeping dst's own. A bare Node
// works as a holder for consumers between two connections.
func MoveChildren(dst, src *Node) { appendChildren(dst, src) }

// appendChildren is MoveNode without dropping the destination's own children.
func appendChildren(dst, src *Node) {
	src.mu.Lock()
	childs := src.childs
	src.childs = nil
	src.mu.Unlock()

	dst.mu.Lock()
	dst.childs = append(dst.childs, childs...)
	dst.mu.Unlock()

	for _, child := range childs {
		child.parent = dst
	}
}
