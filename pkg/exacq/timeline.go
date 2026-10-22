package exacq

import (
	"time"

	"github.com/AlexxIT/go2rtc/pkg/reorder"
	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
)

// Holding limits match pkg/milestone: B-frames follow their keyframe within a
// mini-GOP, so a few pictures or 300 ms decide whether a stream reorders.
const (
	decideTimeout = 300 * time.Millisecond
	maxHold       = 2 * time.Second
)

// frame is an assembled picture plus the output timing the timeline gives it.
type frame struct {
	*native.VideoFrame
	time time.Time // recorder stamp; the output decode time once timed
	// Set by the timeline: presentation is zero when it equals time.
	presentation time.Time
	duration     time.Duration
}

func newFrame(v *native.VideoFrame) *frame {
	return &frame{VideoFrame: v, time: v.Time}
}

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
	bFrames bool

	held    []*frame
	heldAt  time.Time
	decided bool

	pending *frame // I/P: waits for the next timestamp
	reorder *reorder.Reorderer[*frame]
}

func newTimeline(base time.Time, playback bool, signalled float64) *timeline {
	return &timeline{base: base, playback: playback, signalled: signalled}
}

// push accepts the next picture in decode order and returns pictures whose
// output timing is now known.
func (t *timeline) push(f *frame) []*frame {
	t.clock.Observe(f.time.UnixMilli(), f.Key, !f.Picture.B)
	t.bFrames = t.bFrames || f.Picture.B
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
	var out []*frame
	for _, h := range t.held {
		out = append(out, t.emit(h)...)
	}
	t.held = nil
	return out
}

// flush releases every held picture at the end of a finite stream.
func (t *timeline) flush(nominal time.Duration) []*frame {
	var out []*frame
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

func (t *timeline) emit(f *frame) []*frame {
	if t.reorder == nil {
		if !t.bFrames || !f.Key {
			// B-frames first seen mid-GOP are reordered from the next keyframe.
			return t.plain(f)
		}
		var out []*frame
		if t.pending != nil {
			t.pending.duration = max(f.time.Sub(t.pending.time), time.Millisecond)
			out = append(out, t.pending)
			t.pending = nil
		}
		t.reorder = reorder.New[*frame](reorder.Window)
		return append(out, t.reordered(f)...)
	}
	return t.reordered(f)
}

func (t *timeline) plain(f *frame) []*frame {
	previous := t.pending
	t.pending = f
	if previous == nil {
		return nil
	}
	previous.duration = f.time.Sub(previous.time)
	return []*frame{previous}
}

func (t *timeline) reordered(f *frame) []*frame {
	interval := t.intervalOrDefault()
	var out []*frame
	if f.Key {
		out = t.timed(t.reorder.Flush(interval))
		t.reorder.Rebase(float64(f.time.Sub(t.base).Milliseconds()), interval)
	}
	poc := f.Picture.POC
	if !f.Picture.OK {
		poc = int(f.time.Sub(t.base).Milliseconds()) // keep decode order when the header is unreadable
	}
	return append(out, t.timed(t.reorder.Push(f, poc, interval))...)
}

func (t *timeline) timed(in []reorder.Timed[*frame]) []*frame {
	out := make([]*frame, len(in))
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
	return t.clock.Get(t.signalled)
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
