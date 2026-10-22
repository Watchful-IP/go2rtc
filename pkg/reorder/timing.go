// Package reorder restores presentation order for B-frame streams from
// recorders that stamp pictures in decode order. It parses picture order
// counts (H.264, H.265), measures the picture interval per GOP and assigns
// decode and presentation times on a millisecond output timeline.
package reorder

import (
	"sort"
)

// Window covers IBBBP and pyramid B-frame patterns.
const Window = 3

// Timed is an item with its output decode and presentation times, in
// milliseconds on the output timeline.
type Timed[T any] struct {
	Item     T
	DTS, PTS float64
	Duration float64
}

// Reorderer assigns presentation times to B-frame streams. Recorders stamp
// pictures with their arrival time in decode order, which for B-frames is a
// burst per mini-GOP, so presentation order comes from the picture order count.
type Reorderer[T any] struct {
	Window int // pictures held back before the earliest POC is displayed

	pending []*slot[T] // decode order
	nextDTS float64
	nextPTS float64
}

type slot[T any] struct {
	Timed[T]
	poc      int
	assigned bool
}

func New[T any](window int) *Reorderer[T] {
	return &Reorderer[T]{Window: window}
}

// Rebase starts the next GOP no earlier than dts; presentation trails decode
// by the window so composition offsets stay non-negative.
func (r *Reorderer[T]) Rebase(dts, interval float64) {
	r.nextDTS = max(r.nextDTS, dts)
	r.nextPTS = max(r.nextPTS, r.nextDTS+float64(r.Window)*interval)
}

// Push adds a picture in decode order and returns pictures that can be emitted.
func (r *Reorderer[T]) Push(item T, poc int, duration float64) []Timed[T] {
	r.pending = append(r.pending, &slot[T]{Timed: Timed[T]{Item: item, DTS: r.nextDTS, Duration: duration}, poc: poc})
	r.nextDTS += duration
	for r.unassigned() > r.Window {
		r.assignEarliest(duration)
	}
	return r.ready()
}

// Flush displays every held picture; POC restarts at the next IDR.
func (r *Reorderer[T]) Flush(duration float64) []Timed[T] {
	for r.unassigned() > 0 {
		r.assignEarliest(duration)
	}
	return r.ready()
}

func (r *Reorderer[T]) unassigned() (n int) {
	for _, s := range r.pending {
		if !s.assigned {
			n++
		}
	}
	return
}

func (r *Reorderer[T]) assignEarliest(duration float64) {
	var earliest *slot[T]
	for _, s := range r.pending {
		if !s.assigned && (earliest == nil || s.poc < earliest.poc) {
			earliest = s
		}
	}
	earliest.PTS = max(r.nextPTS, earliest.DTS)
	earliest.assigned = true
	r.nextPTS = earliest.PTS + duration
}

// ready pops the decode-order prefix whose presentation time is known.
func (r *Reorderer[T]) ready() []Timed[T] {
	var out []Timed[T]
	for len(r.pending) > 0 && r.pending[0].assigned {
		out = append(out, r.pending[0].Timed)
		r.pending = r.pending[1:]
	}
	return out
}

// GOPClock measures the live picture interval. B-frame streams arrive in
// bursts, but a GOP's keyframes are a whole GOP apart.
type GOPClock struct {
	key         int64 // arrival of the current GOP's keyframe
	pictures    int
	interval    float64
	lastAnchor  int64
	sinceAnchor int
	samples     []float64 // anchor spacing, until a whole GOP is measured
}

// Observe records a picture's recorder time in ms; anchors are non-B pictures.
func (g *GOPClock) Observe(t int64, key, anchor bool) {
	if key {
		if g.key != 0 && t > g.key && g.pictures > 0 {
			g.interval = float64(t-g.key) / float64(g.pictures)
		}
		g.key, g.pictures = t, 0
	}
	g.pictures++
	g.sinceAnchor++
	if anchor {
		if g.lastAnchor != 0 && t > g.lastAnchor && len(g.samples) < 32 {
			g.samples = append(g.samples, float64(t-g.lastAnchor)/float64(g.sinceAnchor))
		}
		g.lastAnchor, g.sinceAnchor = t, 0
	}
}

// Get returns the picture interval in ms, or 0 until one is known: the last
// GOP's span, then the signalled frame rate, then anchor spacing.
func (g *GOPClock) Get(signalled float64) float64 {
	if g.interval >= 1 {
		return g.interval
	}
	if signalled > 0 {
		return signalled
	}
	// One sample can be an I/P pair from the same burst, a millisecond apart.
	if len(g.samples) >= 3 {
		sorted := append([]float64(nil), g.samples...)
		sort.Float64s(sorted)
		if m := sorted[len(sorted)/2]; m >= 1 {
			return m
		}
	}
	return 0
}

// MedianInterval is the typical spacing of a GOP's pictures, ignoring gaps.
func MedianInterval(times []int64) float64 {
	var d []int64
	for i := 1; i < len(times); i++ {
		if delta := times[i] - times[i-1]; delta > 0 {
			d = append(d, delta)
		}
	}
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return float64(d[len(d)/2])
}
