package milestone

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// bFramePattern reproduces how an XProtect recorder stamps an x264 stream with
// three B-frames: each mini-GOP (P then three B in decode order) arrives as a
// burst, and the GOP's P is decoded before the B-frames it anchors.
func bFramePattern(gops int) (times []int64, pocs []int, keys []bool) {
	t := int64(1_790_000_000_000)
	for g := 0; g < gops; g++ {
		times, pocs, keys = append(times, t), append(pocs, 0), append(keys, true)
		for m := 0; m < 12; m++ { // 12 mini-GOPs of 4 pictures plus a closing P
			base := 8*m + 8
			times = append(times, t+1+int64(160*m), t+160+int64(160*m), t+161+int64(160*m), t+162+int64(160*m))
			pocs = append(pocs, base, base-6, base-4, base-2)
			keys = append(keys, false, false, false, false)
		}
		times, pocs, keys = append(times, t+1+160*12), append(pocs, 98), append(keys, false)
		t += 2000
	}
	return
}

func TestReordererPresentsInPOCOrder(t *testing.T) {
	times, pocs, keys := bFramePattern(3)
	var clock gopClock
	r := newReorderer(reorderWindow)
	var out []timedFrame
	for i := range times {
		b := !keys[i] && pocs[i]%8 != 0
		clock.observe(times[i], keys[i], !b)
		interval := clock.get(0)
		if interval == 0 {
			interval = 40
		}
		if keys[i] {
			out = append(out, r.flush(interval)...)
			r.rebase(float64(times[i]-times[0]), interval)
		}
		out = append(out, r.push(frame{time: times[i], key: keys[i], data: []byte{byte(pocs[i])}}, pocs[i], interval)...)
	}
	out = append(out, r.flush(clock.get(0))...)
	require.Len(t, out, len(times))

	for i, f := range out {
		require.GreaterOrEqual(t, f.pts, f.dts, "composition offset must not be negative at %d", i)
		if i > 0 {
			require.Greater(t, f.dts, out[i-1].dts, "decode order at %d", i)
		}
	}

	// In presentation order, pictures within a GOP step evenly by the
	// measured interval and every POC appears in sequence.
	byPTS := append([]timedFrame(nil), out...)
	for i := 1; i < len(byPTS); i++ {
		for j := i; j > 0 && byPTS[j].pts < byPTS[j-1].pts; j-- {
			byPTS[j], byPTS[j-1] = byPTS[j-1], byPTS[j]
		}
	}
	for i := 1; i < len(byPTS); i++ {
		step := byPTS[i].pts - byPTS[i-1].pts
		require.InDelta(t, 40, step, 1.5, "presentation step %d", i)
		if !byPTS[i].key {
			require.Greater(t, int(byPTS[i].data[0]), int(byPTS[i-1].data[0]), "POC order at %d", i)
		}
	}
}

func TestGOPClockIgnoresBurstSpacing(t *testing.T) {
	times, pocs, keys := bFramePattern(2)
	var clock gopClock
	for i := range times {
		clock.observe(times[i], keys[i], keys[i] || pocs[i]%8 == 0)
		if i < 3 {
			require.Zero(t, clock.get(0), "unknown until enough anchors at %d", i)
		}
	}
	require.InDelta(t, 2000.0/50, clock.get(0), 0.01)
	require.Equal(t, 100.0, (&gopClock{}).get(100), "signalled rate before a GOP is measured")
}

func TestReordererKeepsCompositionNonNegative(t *testing.T) {
	// The measured interval shrinks, so presentation would fall behind decode.
	r := newReorderer(1)
	r.rebase(0, 10)
	var out []timedFrame
	for i, d := range []float64{100, 100, 10, 10, 10, 10} {
		out = append(out, r.push(frame{}, i, d)...)
	}
	out = append(out, r.flush(10)...)
	require.Len(t, out, 6)
	for i, f := range out {
		require.GreaterOrEqual(t, f.pts, f.dts, "picture %d", i)
	}
}
