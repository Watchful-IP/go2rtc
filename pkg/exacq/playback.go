package exacq

import "time"

// PlaybackStart selects the latest timed native I picture at or before start,
// or the earliest following I picture if no preceding one was returned. The
// result is stable when used as the start of another whole-second wire search.
// Callers must still check their exclusive end and validate codec parameters.
func PlaybackStart(frames []*MediaPacket, start time.Time) (time.Time, bool) {
	index := playbackStartIndex(frames, start)
	if index < 0 {
		return time.Time{}, false
	}
	return frames[index].Timestamp(), true
}

func playbackStartIndex(frames []*MediaPacket, start time.Time) int {
	before, after := -1, -1
	for i, frame := range frames {
		if frame == nil || frame.Type != MediaIFrame || frame.Timestamp().IsZero() {
			continue
		}
		stamp := frame.Timestamp()
		if !stamp.After(start) {
			if before < 0 || stamp.After(frames[before].Timestamp()) {
				before = i
			}
		} else if after < 0 || stamp.Before(frames[after].Timestamp()) {
			after = i
		}
	}
	if before >= 0 {
		return before
	}
	return after
}
