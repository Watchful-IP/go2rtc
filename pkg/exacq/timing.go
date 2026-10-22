package exacq

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

func (p *Producer) nominalInterval() time.Duration {
	return p.state.NominalInterval(p.stream)
}

// vuiInterval is the stream's signalled picture interval in ms, or 0 when the
// recorder reports no frame rate and the video signals none.
func (p *Producer) vuiInterval() float64 {
	nominal := p.nominalInterval()
	if rate := p.stream.Framerate; (rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0)) && nominal == 40*time.Millisecond {
		return 0 // the assembler's 25 fps fallback, not a signalled rate
	}
	return float64(nominal) / float64(time.Millisecond)
}

func (p *Producer) emit(f *frame, duration time.Duration) error {
	speed := 1.0
	if p.source.Playback {
		speed = p.source.Speed
	}
	relative := f.time.Sub(p.ActualStart)
	dts := math.Round(relative.Seconds() * 90000 / speed)
	// Scale the two boundaries independently, then subtract. Rounding a
	// duration independently can make neighbouring samples overlap by a tick.
	end := math.Round((relative + duration).Seconds() * 90000 / speed)
	dur := end - dts
	pts := dts
	if !f.presentation.IsZero() {
		pts = math.Round(f.presentation.Sub(p.ActualStart).Seconds() * 90000 / speed)
	}
	cts := pts - dts
	if dts < 0 || dts >= math.Exp2(64) || dur > math.MaxUint32 || math.IsInf(dts, 0) || math.IsInf(dur, 0) || cts < 0 || cts > math.MaxUint32 {
		return errors.New("exacq: video timing exceeds clock range")
	}
	p.seq++
	packet := &core.Packet{Header: rtp.Header{Timestamp: uint32(uint64(pts)), SequenceNumber: p.seq, Marker: true}, Payload: f.Data}
	core.SetSampleTiming(packet, core.SampleTiming{ClockID: p.clockID, DecodeTime: uint64(dts), Duration: uint32(max(1, dur)), CompositionOffset: uint32(cts)})
	p.video.Input(packet)
	return nil
}

func waitUntil(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay := time.Until(deadline); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
