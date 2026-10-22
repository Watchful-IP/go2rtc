package exacq

import (
	"time"

	"github.com/AlexxIT/go2rtc/pkg/reorder"
)

// Holding limits match pkg/milestone: B-frames follow their keyframe within a
// mini-GOP, so a few pictures or 300 ms decide whether a stream reorders.
const (
	decideTimeout = 300 * time.Millisecond
	maxHold       = 2 * time.Second
)

// timeline assigns output timing to pictures in decode order.
//
// I/P streams keep the recorder's own timestamps: each picture is released
// when the next one arrives, so its duration is exact even for variable frame
// rates. Recorders stamp B-frame streams in decode order, so their pictures
// are presented by picture order count, spaced at the measured GOP interval
// and re-anchored to the recorder clock at every keyframe (as Milestone does).
type timeline struct {
	playback  bool
	signalled float64 // stream frame interval in ms, 0 if unknown

	base    time.Time // first keyframe; output time zero
	clock   reorder.GOPClock
	parser  *reorder.Parser
	bFrames bool

	held    []*videoFrame
	heldAt  time.Time
	decided bool

	pending *videoFrame // I/P: waits for the next timestamp
	reorder *reorder.Reorderer[*videoFrame]
}

func newTimeline(base time.Time, playback bool, signalled float64, parser *reorder.Parser) *timeline {
	return &timeline{base: base, playback: playback, signalled: signalled, parser: parser}
}

// push accepts the next picture in decode order and returns pictures whose
// output timing is now known.
func (t *timeline) push(f *videoFrame) []*videoFrame {
	t.clock.Observe(f.time.UnixMilli(), f.key, !f.pic.B)
	t.bFrames = t.bFrames || f.pic.B
	if t.decided {
		return t.emit(f)
	}
	if t.held == nil {
		t.heldAt = time.Now()
	}
	t.held = append(t.held, f)
	if t.bFrames {
		// Reordering needs the picture interval; hold until it is known.
		t.decided = t.interval() > 0 || t.heldFor() > maxHold
	} else {
		t.decided = len(t.held) > reorder.Window || t.heldFor() > decideTimeout
	}
	if !t.decided {
		return nil
	}
	var out []*videoFrame
	for _, h := range t.held {
		out = append(out, t.emit(h)...)
	}
	t.held = nil
	return out
}

// flush releases every held picture at the end of a finite stream.
func (t *timeline) flush(nominal time.Duration) []*videoFrame {
	var out []*videoFrame
	for _, h := range t.held {
		out = append(out, t.emit(h)...)
	}
	t.held = nil
	if t.pending != nil {
		t.pending.duration = nominal
		out = append(out, t.pending)
		t.pending = nil
	}
	if t.reorder != nil {
		out = append(out, t.timed(t.reorder.Flush(t.intervalOrDefault()))...)
	}
	return out
}

// heldFor uses wall time live; playback batches arrive at once, so it uses
// the recorder span instead.
func (t *timeline) heldFor() time.Duration {
	if t.playback {
		return t.held[len(t.held)-1].time.Sub(t.held[0].time)
	}
	return time.Since(t.heldAt)
}

func (t *timeline) emit(f *videoFrame) []*videoFrame {
	if t.reorder == nil {
		if !t.bFrames || !f.key {
			// B-frames first seen mid-GOP are reordered from the next keyframe.
			return t.plain(f)
		}
		var out []*videoFrame
		if t.pending != nil {
			t.pending.duration = max(f.time.Sub(t.pending.time), time.Millisecond)
			out = append(out, t.pending)
			t.pending = nil
		}
		t.reorder = reorder.New[*videoFrame](reorder.Window)
		return append(out, t.reordered(f)...)
	}
	return t.reordered(f)
}

func (t *timeline) plain(f *videoFrame) []*videoFrame {
	previous := t.pending
	t.pending = f
	if previous == nil {
		return nil
	}
	previous.duration = f.time.Sub(previous.time)
	return []*videoFrame{previous}
}

func (t *timeline) reordered(f *videoFrame) []*videoFrame {
	interval := t.intervalOrDefault()
	var out []*videoFrame
	if f.key {
		out = t.timed(t.reorder.Flush(interval))
		t.reorder.Rebase(float64(f.time.Sub(t.base).Milliseconds()), interval)
	}
	poc := f.pic.POC
	if !f.pic.OK {
		poc = int(f.time.Sub(t.base).Milliseconds()) // keep decode order when the header is unreadable
	}
	return append(out, t.timed(t.reorder.Push(f, poc, interval))...)
}

func (t *timeline) timed(in []reorder.Timed[*videoFrame]) []*videoFrame {
	out := make([]*videoFrame, len(in))
	for i, timed := range in {
		f := timed.Item
		f.time = t.base.Add(milliseconds(timed.DTS))
		f.presentation = t.base.Add(milliseconds(timed.PTS))
		// Converting the end, not the duration, keeps neighbours contiguous.
		f.duration = t.base.Add(milliseconds(timed.DTS + timed.Duration)).Sub(f.time)
		out[i] = f
	}
	return out
}

func (t *timeline) interval() float64 {
	signalled := t.signalled
	if t.parser != nil && t.parser.Interval > 0 {
		signalled = t.parser.Interval
	}
	return t.clock.Get(signalled)
}

func (t *timeline) intervalOrDefault() float64 {
	if interval := t.interval(); interval > 0 {
		return interval
	}
	return 40 // 25 fps when nothing better is known
}

func milliseconds(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}
