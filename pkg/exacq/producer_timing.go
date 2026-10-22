package exacq

import (
	"context"
	"errors"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"math"
	"time"
)

func (p *Producer) nominalInterval() time.Duration {
	return nominalInterval(&p.state, p.stream)
}

// nominalInterval is the signalled picture interval: H.264 VUI, then the
// recorder's configured frame rate, then 25 fps.
func nominalInterval(state *videoState, stream Stream) time.Duration {
	if state.pictures.Interval > 0 {
		return time.Duration(state.pictures.Interval * float64(time.Millisecond))
	}
	if stream.Framerate > 0 && !math.IsNaN(stream.Framerate) && !math.IsInf(stream.Framerate, 0) {
		return time.Duration(float64(time.Second) / stream.Framerate)
	}
	return 40 * time.Millisecond
}

func (p *Producer) emit(f *videoFrame, duration time.Duration) error {
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
	packet := &core.Packet{Header: rtp.Header{Timestamp: uint32(uint64(pts)), SequenceNumber: p.seq, Marker: true}, Payload: f.data}
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
