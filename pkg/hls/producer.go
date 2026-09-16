package hls

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
)

func OpenURL(u *url.URL, body io.ReadCloser) (core.Producer, error) {
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		body.Close()
		return nil, err
	}
	return open(req, body)
}

// OpenResponse preserves request headers and the final redirected playlist URL.
func OpenResponse(res *http.Response) (core.Producer, error) { return open(res.Request, res.Body) }

func open(req *http.Request, body io.ReadCloser) (core.Producer, error) {
	rd, err := newReader(req, body)
	if err != nil {
		return nil, err
	}
	seg, data, err := rd.nextSegment()
	if err != nil {
		rd.Close()
		return nil, err
	}
	if seg.init.uri == "" {
		if data[0] != 0x47 {
			rd.Close()
			return nil, errors.New("hls: non-TS segment without EXT-X-MAP")
		}
		rd.buf = data
		prod, err := mpegts.Open(rd)
		if err != nil {
			rd.Close()
			return nil, err
		}
		prod.FormatName = "hls/mpegts"
		prod.RemoteAddr = req.URL.Host
		return prod, nil
	}
	init, err := rd.initialization(seg)
	if err != nil {
		rd.Close()
		return nil, err
	}
	dem := &mp4.Demuxer{}
	medias, err := dem.Probe(init)
	if err != nil {
		rd.Close()
		return nil, err
	}
	samples, err := dem.Demux(data)
	if err != nil {
		rd.Close()
		return nil, err
	}
	return &Producer{Connection: core.Connection{ID: core.NewID(), FormatName: "hls/fmp4", RemoteAddr: req.URL.Host, Medias: medias, Transport: rd}, rd: rd, dem: dem, first: samples, segment: seg}, nil
}

type Producer struct {
	core.Connection
	rd      *reader
	dem     *mp4.Demuxer
	first   []mp4.Sample
	segment segment
}

func (p *Producer) Start() error {
	clocks := make(map[uint32]uint32)
	for _, media := range p.Medias {
		codec := media.Codecs[0]
		clocks[p.dem.GetTrackID(codec)] = codec.ClockRate
	}
	receivers := make(map[uint32]*core.Receiver)
	for _, r := range p.Receivers {
		receivers[p.dem.GetTrackID(r.Codec)] = r
	}
	next := make(chan sampleBatch)
	finished := make(chan struct{})
	go func() { defer close(finished); p.readSegments(next) }()
	defer func() { p.rd.Close(); <-finished }()
	samples := p.first
	p.first = nil
	var epoch time.Duration
	var start time.Time
	var periodOffset, timelineEnd time.Duration
	reset := true
	lastDecode := make(map[uint32]uint64)
	origins := make(map[uint32]trackOrigin)
	for {
		times := make(map[*core.Packet]time.Duration, len(samples))
		for _, s := range samples {
			if last, ok := lastDecode[s.TrackID]; ok && s.DecodeTime < last {
				return errors.New("hls: decode timeline moved backwards without discontinuity")
			}
			lastDecode[s.TrackID] = s.DecodeTime + uint64(s.Duration)
			seconds := s.DecodeTime / uint64(s.TimeScale)
			if seconds > uint64(math.MaxInt64/int64(time.Second))-1 {
				return errors.New("hls: decode time out of range")
			}
			times[s.Packet] = time.Duration(seconds)*time.Second + time.Duration(s.DecodeTime%uint64(s.TimeScale))*time.Second/time.Duration(s.TimeScale)
		}
		sort.SliceStable(samples, func(i, j int) bool { return times[samples[i].Packet] < times[samples[j].Packet] })
		if start.IsZero() {
			start = time.Now()
		}
		if reset {
			epoch = times[samples[0].Packet]
			periodOffset = timelineEnd
			reset = false
		}
		for _, s := range samples {
			elapsed := periodOffset + times[s.Packet] - epoch
			if elapsed < 0 || elapsed-time.Since(start) > time.Minute {
				return errors.New("hls: timestamp discontinuity; reconnect required")
			}
			if err := p.rd.wait(elapsed - time.Since(start)); err != nil {
				return err
			}
			timelineEnd = max(timelineEnd, elapsed+time.Duration(s.Duration)*time.Second/time.Duration(s.TimeScale))
			// Anchor once per track, then retain integer source-clock deltas.
			timing, _ := core.GetSampleTiming(s.Packet)
			origin, ok := origins[s.TrackID]
			if !ok {
				clock := uint64(clocks[s.TrackID])
				origin = trackOrigin{timing.DecodeTime, uint64(elapsed)/uint64(time.Second)*clock + uint64(elapsed)%uint64(time.Second)*clock/uint64(time.Second)}
				origins[s.TrackID] = origin
			}
			timing.DecodeTime = origin.output + timing.DecodeTime - origin.input
			timing.ClockID = p.ID
			core.SetSampleTiming(s.Packet, timing)
			s.Packet.Timestamp = uint32(timing.DecodeTime) + timing.CompositionOffset

			p.Recv += len(s.Packet.Payload)
			if r := receivers[s.TrackID]; r != nil {
				r.WriteRTP(s.Packet)
			}
		}
		select {
		case <-p.rd.ctx.Done():
			return p.rd.ctx.Err()
		case batch := <-next:
			if batch.err != nil {
				return batch.err
			}
			if batch.discontinuity {
				reset = true
				lastDecode = make(map[uint32]uint64)
				origins = make(map[uint32]trackOrigin)
			}
			samples = batch.samples
		}
	}
}

type sampleBatch struct {
	samples       []mp4.Sample
	discontinuity bool
	err           error
}

// One segment is prefetched while the current one is paced. The unbuffered
// handoff bounds memory and prevents narrow live playlists outrunning playback.
func (p *Producer) readSegments(out chan<- sampleBatch) {
	for {
		batch := p.readSegment()
		select {
		case <-p.rd.ctx.Done():
			return
		case out <- batch:
		}
		if batch.err != nil {
			return
		}
	}
}

func (p *Producer) readSegment() sampleBatch {
	seg, data, err := p.rd.nextSegment()
	if err != nil {
		return sampleBatch{err: err}
	}
	discontinuity := seg.discontinuity != p.segment.discontinuity
	if discontinuity {
		p.rd.init = resource{}
	}
	if discontinuity || seg.init != p.segment.init || seg.initKey != p.segment.initKey {
		init, err := p.rd.initialization(seg)
		if err != nil {
			return sampleBatch{err: err}
		}
		next := &mp4.Demuxer{}
		if _, err = next.Probe(init); err != nil {
			return sampleBatch{err: err}
		}
		if !p.dem.Compatible(next) {
			return sampleBatch{err: errors.New("hls: track configuration changed; reconnect required")}
		}
		p.dem = next
	}
	p.segment = seg
	samples, err := p.dem.Demux(data)
	return sampleBatch{samples, discontinuity, err}
}

type trackOrigin struct{ input, output uint64 }
