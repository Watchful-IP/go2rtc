package exacq

import (
	"sync"
	"time"
)

// Trace accumulates per-request diagnostics across every session a request
// opens (control, worker and verification). It is safe for concurrent use and
// all methods accept a nil receiver.
type Trace struct {
	mu       sync.Mutex
	order    []string
	phases   map[string]time.Duration
	connects int
	serial   string
	version  string
}

type PhaseTiming struct {
	Phase    string
	Duration time.Duration
}

// Observe adds the time since start to phase.
func (t *Trace) Observe(phase string, start time.Time) {
	if t == nil {
		return
	}
	d := time.Since(start)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phases == nil {
		t.phases = make(map[string]time.Duration)
	}
	if _, found := t.phases[phase]; !found {
		t.order = append(t.order, phase)
	}
	t.phases[phase] += d
}

// Phases returns cumulative durations in first-seen order.
func (t *Trace) Phases() []PhaseTiming {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PhaseTiming, 0, len(t.order))
	for _, phase := range t.order {
		out = append(out, PhaseTiming{phase, t.phases[phase]})
	}
	return out
}

// Connects counts dial attempts, including failed ones.
func (t *Trace) Connects() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connects
}

// Recorder reports the serial from discovery and the version from configuration.
func (t *Trace) Recorder() (serial, version string) {
	if t == nil {
		return "", ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.serial, t.version
}

func (t *Trace) connect() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.connects++
	t.mu.Unlock()
}

func (t *Trace) identify(serial, version string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if serial != "" {
		t.serial = serial
	}
	if version != "" {
		t.version = version
	}
}
