package milestone

import "github.com/AlexxIT/go2rtc/pkg/reorder"

const clockRate = 90 // ticks per millisecond

const reorderWindow = reorder.Window

// timedFrame is a frame with its output decode and presentation times, in
// milliseconds on the output timeline.
type timedFrame struct {
	frame
	dts, pts float64
	duration float64
}

// reorderer adapts the shared reorder.Reorderer to Milestone frames; playback
// widens window to a whole GOP.
type reorderer struct {
	window int
	r      reorder.Reorderer[frame]
}

func newReorderer(window int) *reorderer {
	return &reorderer{window: window}
}

func (r *reorderer) rebase(dts, interval float64) {
	r.r.Window = r.window
	r.r.Rebase(dts, interval)
}

func (r *reorderer) push(f frame, poc int, duration float64) []timedFrame {
	r.r.Window = r.window
	return timedFrames(r.r.Push(f, poc, duration))
}

func (r *reorderer) flush(duration float64) []timedFrame {
	return timedFrames(r.r.Flush(duration))
}

func timedFrames(in []reorder.Timed[frame]) []timedFrame {
	out := make([]timedFrame, len(in))
	for i, t := range in {
		out[i] = timedFrame{frame: t.Item, dts: t.DTS, pts: t.PTS, duration: t.Duration}
	}
	return out
}

type gopClock struct{ clock reorder.GOPClock }

func (g *gopClock) observe(t int64, key, anchor bool) { g.clock.Observe(t, key, anchor) }

func (g *gopClock) get(signalled float64) float64 { return g.clock.Get(signalled) }

func medianInterval(times []int64) float64 { return reorder.MedianInterval(times) }
