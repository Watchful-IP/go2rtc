package exacq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// Opt-in hardware metric. Source credentials are read from a private file and
// are never emitted. Frame age is relative to the recorder clock, not a
// glass-to-glass latency measurement; recorder clock skew affects the result.
func TestLabProducerTiming(t *testing.T) {
	u := producerLabURL(t)
	if os.Getenv("EXACQ_TEST_PRODUCER_LOWEST") != "" {
		u.RawQuery = "select=lowest"
	}
	camera := u.Path[1:]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := time.Now()
	var firstFrame time.Duration
	producer, err := DialContext(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	p := producer.(*Producer)
	defer p.Stop()
	probeTime := time.Since(before)
	var ages []float64
	var bytes int
	var lastEnd uint64
	var timingError bool
	p.video.Input = func(packet *core.Packet) {
		if firstFrame == 0 {
			firstFrame = time.Since(before)
		}
		timing, ok := core.GetSampleTiming(packet)
		if !ok || (lastEnd != 0 && timing.DecodeTime != lastEnd) {
			timingError = true
		}
		lastEnd = timing.DecodeTime + uint64(timing.Duration)
		stamp := p.ActualStart.Add(time.Duration(timing.DecodeTime) * time.Second / 90000)
		ages = append(ages, float64(time.Since(stamp))/float64(time.Millisecond))
		bytes += len(packet.Payload)
	}
	var initial, final runtime.MemStats
	runtime.ReadMemStats(&initial)
	started := time.Now()
	timer := time.AfterFunc(3*time.Second, func() { _ = p.Stop() })
	err = p.Start()
	timer.Stop()
	elapsed := time.Since(started)
	runtime.ReadMemStats(&final)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if timingError {
		t.Fatal("overlapping, missing or discontinuous sample timing")
	}
	if len(ages) == 0 {
		t.Fatal("no emitted frames")
	}
	sort.Float64s(ages)
	t.Logf("stream=%d %dx%d first_frame_ms=%.1f", p.stream.ID, p.stream.Width, p.stream.Height, float64(firstFrame)/float64(time.Millisecond))
	t.Logf("camera=%s codec=%s frames=%d probe_ms=%.1f output_fps=%.2f throughput_mbps=%.3f age_ms_min=%.1f median=%.1f p95=%.1f max=%.1f allocated_bytes_per_frame=%d",
		camera, p.video.Codec.Name, len(ages), float64(probeTime)/float64(time.Millisecond), float64(len(ages))/elapsed.Seconds(), float64(bytes)*8/elapsed.Seconds()/1e6,
		ages[0], ages[len(ages)/2], ages[(len(ages)-1)*95/100], ages[len(ages)-1], (final.TotalAlloc-initial.TotalAlloc)/uint64(len(ages)))
}

func producerLabURL(t *testing.T) *url.URL {
	t.Helper()
	camera := os.Getenv("EXACQ_TEST_PRODUCER_CAMERA")
	address := os.Getenv("EXACQ_TEST_ADDRESS")
	credentialFile := os.Getenv("EXACQ_TEST_CREDENTIALS")
	if camera == "" || address == "" || credentialFile == "" {
		t.Skip("set explicit producer camera, lab address and private credential file")
	}
	data, err := os.ReadFile(credentialFile)
	if err != nil {
		t.Fatal("read private credential file")
	}
	var item struct{ Fields []struct{ ID, Value string } }
	if json.Unmarshal(data, &item) != nil {
		t.Fatal("decode private credential file")
	}
	var username, password string
	for _, field := range item.Fields {
		switch field.ID {
		case "username":
			username = field.Value
		case "password":
			password = field.Value
		}
	}
	return &url.URL{Scheme: "exacq", Host: address, Path: "/" + camera, User: url.UserPassword(username, password)}
}

func TestLabProducerRange(t *testing.T) {
	start, end := os.Getenv("EXACQ_TEST_PRODUCER_START"), os.Getenv("EXACQ_TEST_PRODUCER_END")
	if start == "" || end == "" {
		t.Skip("set explicit finite range for producer validation")
	}
	u := producerLabURL(t)
	u.RawQuery = url.Values{"start": {start}, "end": {end}, "speed": {"4"}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := time.Now()
	producer, err := DialContext(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	p := producer.(*Producer)
	defer p.Stop()
	count := 1
	last := p.ActualStart
	var gap time.Duration
	for {
		frame, err := p.next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("after %d frames, last timestamp %d: %v", count, last.UnixMilli(), err)
		}
		gap = max(gap, frame.time.Sub(last))
		last = frame.time
		count++
	}
	t.Logf("frames=%d actual_start_ms=%d last_ms=%d max_gap_ms=%d fetch_ms=%.1f", count, p.ActualStart.UnixMilli(), last.UnixMilli(), gap.Milliseconds(), float64(time.Since(before))/float64(time.Millisecond))
}
