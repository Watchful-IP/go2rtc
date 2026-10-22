package exacq

import (
	"strconv"
	"sync"
	"time"
)

// silentStreams remembers enabled streams that sent no video, per recorder
// serial (addresses change under the platform's per-session SOCKS listener),
// so select=lowest tries them last instead of waiting on them every view.
var silentStreams = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

const (
	silentStreamMemory = 10 * time.Minute
	maxSilentStreams   = 4096
)

func silentKey(serial string, stream uint32) string {
	return serial + "/" + strconv.FormatUint(uint64(stream), 10)
}

func markSilent(serial string, stream uint32) {
	silentStreams.Lock()
	defer silentStreams.Unlock()
	now := time.Now()
	if len(silentStreams.until) >= maxSilentStreams {
		for key, until := range silentStreams.until {
			if now.After(until) {
				delete(silentStreams.until, key)
			}
		}
		if len(silentStreams.until) >= maxSilentStreams {
			return
		}
	}
	silentStreams.until[silentKey(serial, stream)] = now.Add(silentStreamMemory)
}

// preferResponsive moves recently silent streams to the end, keeping order.
func preferResponsive(serial string, streams []Stream) []Stream {
	silentStreams.Lock()
	defer silentStreams.Unlock()
	now := time.Now()
	ordered := make([]Stream, 0, len(streams))
	var silent []Stream
	for _, stream := range streams {
		if until, ok := silentStreams.until[silentKey(serial, stream.ID)]; ok && now.Before(until) {
			silent = append(silent, stream)
		} else {
			ordered = append(ordered, stream)
		}
	}
	return append(ordered, silent...)
}
