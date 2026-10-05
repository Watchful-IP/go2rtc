package milestone

import (
	"context"
	"errors"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

const (
	// GOPs requested ahead; each next returns one GOP, so this hides tunnel latency.
	pipelineGOPs = 3
	// Recorder-transcoded JPEG returns one picture per request.
	pipelineJPEG = 12
	// Gaps between recorded sequences are closed rather than waited out.
	maxFrameGap = 1000
)

// The newest GOP reaches the database about a second after it is recorded.
var (
	edgePoll    = 500 * time.Millisecond
	edgeTimeout = 10 * time.Second
)

// Playback streams recorded video from start to end at the requested speed.
// The ImageServer only answers goto/next with one GOP each, so requests are
// pipelined and the pictures re-paced here.
type Playback struct {
	stream
	ctx    context.Context
	cancel context.CancelFunc

	first    *response // the GOP that answered goto, held from probe
	lastGOP  int64
	inflight int
	window   int

	out      float64 // output decode time of the next picture, ms
	interval float64 // typical picture spacing, ms
	reorder  *reorderer
	bMode    bool

	started time.Time
	emitted bool
}

func dialPlayback(src *Source) (*Playback, error) {
	jpeg := src.JPEG
	for {
		client, err := dialClient(src, jpeg)
		if err != nil {
			return nil, err
		}
		p := &Playback{}
		p.init(src, client, "milestone/playback")
		err = p.probe()
		if err == nil {
			p.ctx, p.cancel = context.WithCancel(context.Background())
			return p, nil
		}
		_ = client.Close()
		if !jpeg && (errors.Is(err, errUnsupported) || errors.Is(err, errVideoBlock)) {
			jpeg = true
			continue
		}
		return nil, err
	}
}

func (p *Playback) probe() error {
	if err := p.client.Goto(p.src.Start); err != nil {
		return err
	}
	start := p.src.Start.UnixMilli()
	var stalledSince time.Time
	for {
		res, err := p.readImage()
		if err != nil {
			return err
		}
		fs, err := p.frames(res)
		if err != nil {
			return err
		}
		if len(fs) == 0 {
			return errNoRecording
		}
		// A goto into a gap answers with the last GOP before it; footage after
		// the gap, or footage still being written, follows on next.
		if fs[len(fs)-1].time+maxFrameGap >= start {
			return p.accept(res, fs)
		}
		if cur := res.int64("current"); cur > p.lastGOP {
			p.lastGOP = cur
			stalledSince = time.Time{}
		} else if stalledSince.IsZero() {
			stalledSince = time.Now()
		} else if time.Since(stalledSince) > edgeTimeout {
			return errNoRecording
		} else {
			time.Sleep(edgePoll)
		}
		if err = p.client.Next(); err != nil {
			return err
		}
	}
}

var errNoRecording = errors.New("milestone: no recording in the requested range")

func (p *Playback) accept(res *response, fs []frame) error {
	if !p.src.End.IsZero() && fs[0].time >= p.src.End.UnixMilli() {
		return errNoRecording
	}
	for _, f := range fs {
		if f.key {
			if err := p.addTrack(&f); err != nil {
				return err
			}
			break
		}
	}
	if p.video == nil {
		return errors.New("milestone: recording has no keyframe")
	}
	p.video.Lossless = true // finite: apply backpressure rather than drop
	p.first = res
	p.window = pipelineGOPs
	if p.codec == codecJPEG && len(fs) == 1 {
		p.window = pipelineJPEG
	}
	return nil
}

// readImage skips XML documents until the next image response.
func (p *Playback) readImage() (*response, error) {
	for {
		res, err := p.client.read()
		if err != nil {
			return nil, err
		}
		if !res.isXML() {
			return res, nil
		}
		if xmlValue(res.xml, "methodname") == "connectupdate" {
			continue
		}
	}
}

func (p *Playback) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return p.video, nil
}

func (p *Playback) IsFinite() bool { return true }

func (p *Playback) Start() (err error) {
	defer func() {
		_ = p.client.Close()
		if errors.Is(err, errEnd) {
			err = nil
		}
		p.video.End(err)
	}()

	p.started = time.Now()
	res := p.first
	p.first = nil
	var stalledSince time.Time
	for {
		if cur := res.int64("current"); cur > p.lastGOP || !p.emitted {
			if err = p.gop(res); err != nil {
				return err
			}
			p.lastGOP = max(p.lastGOP, cur)
			stalledSince = time.Time{}
		} else if stalledSince.IsZero() {
			// next repeats the newest GOP at the end of the database.
			stalledSince = time.Now()
		}
		if stalledSince.IsZero() {
			for ; p.inflight < p.window; p.inflight++ {
				if err = p.client.Next(); err != nil {
					return err
				}
			}
		} else if p.inflight == 0 {
			// Poll one request at a time until footage is written or the edge times out.
			if time.Since(stalledSince) > edgeTimeout {
				return nil
			}
			if err = p.sleep(edgePoll); err != nil {
				return err
			}
			if err = p.client.Next(); err != nil {
				return err
			}
			p.inflight++
		}
		if res, err = p.readImage(); err != nil {
			return err
		}
		p.inflight--
	}
}

var errEnd = errors.New("milestone: end of range")

// gop emits one response's pictures.
func (p *Playback) gop(res *response) error {
	fs, err := p.frames(res)
	if err != nil {
		return err
	}
	if len(fs) == 0 {
		return nil
	}
	next := res.int64("next")

	pics := make([]picture, len(fs))
	for i := range fs {
		if fs[i].codec != p.codec {
			return errors.New("milestone: codec changed during playback")
		}
		fs[i].data = p.payload(&fs[i])
		if p.codec != codecJPEG {
			pics[i] = p.pictures.parse(fs[i].data)
			p.bMode = p.bMode || pics[i].b
		}
	}

	end := p.src.End.UnixMilli()
	if p.bMode {
		return p.reordered(fs, pics, next, end)
	}
	times := make([]int64, len(fs))
	for i, f := range fs {
		times[i] = f.time
	}
	if m := medianInterval(times); m > 0 {
		p.interval = m
	} else if p.interval == 0 {
		p.interval = 40
	}
	for i, f := range fs {
		if !p.src.End.IsZero() && f.time >= end {
			return errEnd
		}
		if !p.emitted && !f.key {
			continue
		}
		after := next
		if i+1 < len(fs) {
			after = fs[i+1].time
		}
		if err = p.emit(f, p.out, p.out, p.duration(after-f.time)); err != nil {
			return err
		}
	}
	return nil
}

// reordered emits a B-frame GOP. Arrival times bunch up per mini-GOP, so
// pictures are spread evenly over the GOP and presented in POC order.
func (p *Playback) reordered(fs []frame, pics []picture, next, end int64) error {
	// The GOP spans keyframe to Next unless Next lies across a recording gap;
	// then the previous GOP's spacing is kept.
	if span := next - fs[0].time; span > 0 && span <= int64(len(fs))*maxFrameGap {
		p.interval = float64(span) / float64(len(fs))
	} else if p.interval == 0 {
		var clock gopClock
		for i, f := range fs {
			clock.observe(f.time, f.key, !pics[i].b)
		}
		if p.interval = clock.get(p.pictures.interval); p.interval == 0 {
			p.interval = 40
		}
	}
	interval := p.interval
	if p.reorder == nil {
		p.reorder = newReorderer(reorderWindow)
		p.reorder.rebase(p.out, interval)
	}
	// The whole GOP is at hand, so it is ordered in one pass.
	p.reorder.window = len(fs)
	var ready []timedFrame
	for i, f := range fs {
		if !p.emitted && i == 0 && !f.key {
			return nil
		}
		poc := pics[i].poc
		if !pics[i].ok {
			poc = i
		}
		if pics[i].reset {
			ready = append(ready, p.reorder.flush(interval)...)
		}
		ready = append(ready, p.reorder.push(f, poc, interval)...)
	}
	ready = append(ready, p.reorder.flush(interval)...)
	for _, t := range ready {
		if !p.src.End.IsZero() && t.time >= end {
			return errEnd
		}
		if err := p.emit(t.frame, t.dts, t.pts, t.duration); err != nil {
			return err
		}
	}
	return nil
}

func (p *Playback) duration(delta int64) float64 {
	if delta <= 0 || float64(delta) > max(3*p.interval, maxFrameGap) {
		return p.interval
	}
	return float64(delta)
}

// emit paces a picture to its decode time and writes it with explicit timing.
// Times are divided by speed, so a player at 1x shows the requested rate.
func (p *Playback) emit(f frame, dts, pts, duration float64) error {
	p.emitted = true
	p.out = dts + duration
	dts, pts, duration = dts/p.src.Speed, pts/p.src.Speed, duration/p.src.Speed
	if wait := time.Duration(dts*float64(time.Millisecond)) - time.Since(p.started); wait > 0 {
		if err := p.sleep(wait); err != nil {
			return err
		}
	}
	p.Recv += len(f.data)
	packet := &core.Packet{Header: rtp.Header{Timestamp: uint32(pts * clockRate)}, Payload: f.data}
	core.SetSampleTiming(packet, core.SampleTiming{
		ClockID:           p.ID,
		DecodeTime:        uint64(dts * clockRate),
		Duration:          max(uint32(duration*clockRate), 1),
		CompositionOffset: uint32((pts - dts) * clockRate),
	})
	p.video.Input(packet)
	return p.ctx.Err()
}

func (p *Playback) sleep(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *Playback) Stop() error {
	p.cancel()
	p.video.End(context.Canceled)
	return p.Connection.Stop()
}
