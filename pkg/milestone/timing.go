package milestone

import (
	"sort"
)

const clockRate = 90 // ticks per millisecond

// reorderWindow covers IBBBP and pyramid B-frame patterns.
const reorderWindow = 3

// timedFrame is a frame with its output decode and presentation times, in
// milliseconds on the output timeline.
type timedFrame struct {
	frame
	dts, pts float64
	duration float64
}

// reorderer assigns presentation times to B-frame streams. The recorder stamps
// pictures with their arrival time in decode order, which for B-frames is a
// burst per mini-GOP, so presentation order comes from the picture order count.
type reorderer struct {
	window int // pictures held back before the earliest POC is displayed

	pending []*reorderSlot // decode order
	nextDTS float64
	nextPTS float64
}

type reorderSlot struct {
	timedFrame
	poc      int
	assigned bool
}

func newReorderer(window int) *reorderer {
	return &reorderer{window: window}
}

// rebase starts the next GOP no earlier than dts; presentation trails decode
// by the window so composition offsets stay non-negative.
func (r *reorderer) rebase(dts, interval float64) {
	r.nextDTS = max(r.nextDTS, dts)
	r.nextPTS = max(r.nextPTS, r.nextDTS+float64(r.window)*interval)
}

// push adds a picture in decode order and returns pictures that can be emitted.
func (r *reorderer) push(f frame, poc int, duration float64) []timedFrame {
	r.pending = append(r.pending, &reorderSlot{timedFrame: timedFrame{frame: f, dts: r.nextDTS, duration: duration}, poc: poc})
	r.nextDTS += duration
	for r.unassigned() > r.window {
		r.assignEarliest(duration)
	}
	return r.ready()
}

// flush displays every held picture; POC restarts at the next IDR.
func (r *reorderer) flush(duration float64) []timedFrame {
	for r.unassigned() > 0 {
		r.assignEarliest(duration)
	}
	return r.ready()
}

func (r *reorderer) unassigned() (n int) {
	for _, s := range r.pending {
		if !s.assigned {
			n++
		}
	}
	return
}

func (r *reorderer) assignEarliest(duration float64) {
	var earliest *reorderSlot
	for _, s := range r.pending {
		if !s.assigned && (earliest == nil || s.poc < earliest.poc) {
			earliest = s
		}
	}
	earliest.pts = max(r.nextPTS, earliest.dts)
	earliest.assigned = true
	r.nextPTS = earliest.pts + duration
}

// ready pops the decode-order prefix whose presentation time is known.
func (r *reorderer) ready() []timedFrame {
	var out []timedFrame
	for len(r.pending) > 0 && r.pending[0].assigned {
		out = append(out, r.pending[0].timedFrame)
		r.pending = r.pending[1:]
	}
	return out
}

// gopClock measures the live picture interval. B-frame streams arrive in
// bursts, but a GOP's keyframes are a whole GOP apart.
type gopClock struct {
	key         int64 // arrival of the current GOP's keyframe
	pictures    int
	interval    float64
	lastAnchor  int64
	sinceAnchor int
	samples     []float64 // anchor spacing, until a whole GOP is measured
}

func (g *gopClock) observe(t int64, key, anchor bool) {
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

// get returns the picture interval in ms, or 0 until one is known: the last
// GOP's span, then the signalled frame rate, then anchor spacing.
func (g *gopClock) get(signalled float64) float64 {
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

// medianInterval is the typical spacing of a GOP's pictures, ignoring gaps.
func medianInterval(times []int64) float64 {
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
