package exacq

import "time"

const (
	maxBufferedPictures = 512
	maxBufferedBytes    = 32 << 20
)

// liveStart times the recorder's buffered GOP. A live request is answered at
// once with the current GOP so far (keyframe first, timestamps zero), then
// timed pictures continue that GOP. When picture order shows the buffer runs
// straight into the first timed picture, the buffer is back-dated from it at
// the nominal interval and output starts immediately. Otherwise the buffer is
// dropped and output waits for the next timed keyframe.
type liveStart struct {
	buffered []*videoFrame
	bytes    int
	timed    bool // the first timed picture has arrived
	waitKey  bool // the buffer was unusable: skip to the next keyframe
}

// push takes pictures in decode order and returns those ready for output.
func (l *liveStart) push(f *videoFrame, interval time.Duration) []*videoFrame {
	if f.time.IsZero() {
		if l.timed {
			return nil // untimed pictures after the buffer cannot be placed
		}
		if len(l.buffered) == 0 && !f.key {
			return nil
		}
		l.bytes += len(f.data)
		if len(l.buffered) >= maxBufferedPictures || l.bytes > maxBufferedBytes {
			l.buffered, l.bytes = nil, 0
			return nil
		}
		l.buffered = append(l.buffered, f)
		return nil
	}
	if !l.timed {
		l.timed = true
		buffered := l.buffered
		l.buffered, l.bytes = nil, 0
		if !f.key {
			if !continues(buffered, f) {
				l.waitKey = true
				return nil
			}
			for i, b := range buffered {
				b.time = f.time.Add(-time.Duration(len(buffered)-i) * interval)
				b.backdated = true
			}
			return append(buffered, f)
		}
	}
	if l.waitKey {
		if !f.key {
			return nil
		}
		l.waitKey = false
	}
	return []*videoFrame{f}
}

// continues reports whether the timed picture directly follows the buffer.
// Reordered (B-frame) buffers are not used: their decode order cannot show it.
func continues(buffered []*videoFrame, f *videoFrame) bool {
	if len(buffered) == 0 || !buffered[0].key || !f.pic.OK || f.pic.B {
		return false
	}
	step := 0
	for i, b := range buffered {
		if !b.pic.OK || b.pic.B {
			return false
		}
		if i > 0 {
			delta := b.pic.POC - buffered[i-1].pic.POC
			if delta <= 0 {
				return false
			}
			step = max(step, delta)
		}
	}
	if step == 0 {
		step = 2 // a lone keyframe: H.264 frame POC advances by 2, HEVC by 1
	}
	delta := f.pic.POC - buffered[len(buffered)-1].pic.POC
	return delta > 0 && delta <= step
}
