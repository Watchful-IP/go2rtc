package exacq

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
)

// Dial opens a native Exacq video source. Playback is finite and aligns its
// start to the available preceding keyframe required for decoding.
func Dial(rawURL string) (core.Producer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return DialContext(ctx, rawURL)
}

// DialContext uses ctx for connection and initial media probing. Once it
// returns, Stop owns cancellation of the established producer.
func DialContext(ctx context.Context, rawURL string) (core.Producer, error) {
	source, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	client, err := native.Connect(ctx, source.options(source.liveStream()))
	if err != nil {
		return nil, err
	}
	serial, _ := client.Recorder()
	candidates, err := streamCandidates(client.Inventory(), source)
	if err == nil {
		candidates = preferResponsive(serial, candidates)
	}
	for i := 0; err == nil; i++ {
		var p *Producer
		p, err = dialStream(ctx, source, client, candidates[i], i < len(candidates)-1)
		if err == nil {
			return p, nil
		}
		_ = client.Close()
		if !errors.Is(err, errSilentStream) {
			return nil, licenseReason(ctx, source, err)
		}
		markSilent(serial, candidates[i].ID)
		if i == len(candidates)-1 {
			return nil, licenseReason(ctx, source, err)
		}
		// A configured, enabled stream can still send nothing (an unconnected
		// substream). Interrupting its read closed the session; try the next.
		if client, err = native.Connect(ctx, source.options(candidates[i+1].ID)); err != nil {
			return nil, err
		}
	}
	_ = client.Close()
	return nil, err
}

// licenseReason replaces a failed start's error with the recorder's licence
// problem, if it has one. The licence is read only after a failure, so a
// working stream never waits for it.
func licenseReason(ctx context.Context, source *Source, failure error) error {
	client, err := native.Connect(ctx, source.options(0))
	if err != nil {
		return failure
	}
	defer client.Close()
	if err = native.CheckLicense(ctx, client); err != nil {
		if e := native.AsError(err); e != nil && e.Phase == native.PhaseLicense && e.Code == native.CodeStreamUnavailable {
			return err
		}
	}
	return failure
}

// silentStreamTimeout bounds waiting for a stream's first packet when other
// streams are left to try. A live stream answers within milliseconds with the
// recorder's buffered GOP; once video flows, waiting for a keyframe is normal.
var silentStreamTimeout = 4 * time.Second

var (
	errSilentStream = errors.New("exacq: live stream sent no video")
	errNoVideoYet   = errors.New("exacq: no video before the silence timeout")
)

func dialStream(ctx context.Context, source *Source, client native.MediaSession, stream native.Stream, fallback bool) (*Producer, error) {
	probeCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	p := newProducer(source, client, stream, source.URL.Host)
	if fallback && !source.Playback { // a recorded search returns its packets only once complete
		silence := time.AfterFunc(silentStreamTimeout, func() { cancel(errNoVideoYet) })
		defer silence.Stop()
		p.onVideo = func() { silence.Stop() }
	}
	if err := p.probe(probeCtx); err != nil {
		p.cancel()
		return nil, err
	}
	return p, nil
}

type Producer struct {
	core.Connection
	// ActualStart is the timestamp of the first decodable picture. Playback
	// can start before the requested range to retain necessary GOP preroll.
	ActualStart time.Time `json:"actual_start"`

	source    *Source
	client    native.MediaSession
	stream    native.Stream
	video     *core.Receiver
	state     native.VideoAssembler
	assembled bool // the assembler has produced a decodable picture
	timeline  *timeline
	start     native.LiveStart
	queued    []*frame // live pictures released by start, decode order
	ready     []*frame
	ended     bool
	ctx       context.Context
	cancel    context.CancelFunc
	started   atomic.Bool
	clockID   uint32
	seq       uint16
	lastKey   time.Time
	last      time.Time

	onVideo func() // called once, on the live stream's first packet

	cursor        time.Time
	frames        []*native.MediaPacket
	index         int
	previousBatch map[[32]byte]struct{}
}

func newProducer(source *Source, client native.MediaSession, stream native.Stream, address string) *Producer {
	p := &Producer{source: source, client: client, stream: stream, state: native.VideoAssembler{Format: stream.Format, KeepUntimed: !source.Playback}, cursor: source.Start, clockID: core.NewID()}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	format := "exacq"
	if source.Playback {
		format += "/playback"
	}
	p.Connection = core.Connection{ID: core.NewID(), FormatName: format, Protocol: "tcp", RemoteAddr: address, URL: source.Redacted(), Transport: client}
	return p
}

// streamCandidates is the stream to play, or for select=lowest every enabled
// H.264/H.265 stream of the camera, smallest first.
func streamCandidates(inventory *native.Inventory, source *Source) ([]native.Stream, error) {
	if source.SelectLowest {
		return native.LowestStreams(inventory, source.Camera)
	}
	stream, err := native.SelectStream(inventory, source.Camera, source.Stream)
	return []native.Stream{stream}, err
}

// errPlaybackEnd is the producer's own end of a finite stream. Session errors
// are never treated as an end: a dropped recorder connection keeps io.EOF in
// its chain, and must fail the clip rather than truncate it silently.
var errPlaybackEnd = errors.New("exacq: end of playback")

// probe waits for the first decodable keyframe, which defines the track.
func (p *Producer) probe(ctx context.Context) error {
	if !p.source.Playback {
		if err := p.client.StartLive(p.stream.ID); err != nil {
			return err
		}
	}
	f, err := p.next(ctx)
	if err != nil {
		if errors.Is(err, errPlaybackEnd) {
			return failf(native.CodeNoRecording, native.PhaseSearch, "no decodable recording in the requested range")
		}
		typed := native.AsError(err)
		silent := errors.Is(context.Cause(ctx), errNoVideoYet) || errors.Is(err, context.DeadlineExceeded) || typed != nil && typed.Code == native.CodeTimeout
		if !p.source.Playback && silent {
			return wrapf(errors.Join(errSilentStream, err), native.CodeStreamUnavailable, native.PhaseLive, "camera %d sent no video on stream %d (%dx%d); it may be offline or disconnected from the recorder", p.source.Camera, p.stream.ID, p.stream.Width, p.stream.Height)
		}
		return err
	}
	p.ActualStart = f.time
	signalled := 0.0
	if rate := p.stream.Framerate; rate > 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0) {
		signalled = 1000 / rate
	}
	if vui := p.vuiInterval(); vui > 0 {
		signalled = vui
	}
	p.timeline = newTimeline(f.time, p.source.Playback, signalled)
	p.ready = p.timeline.push(f)
	codec := videoCodec(p.stream.Format, f.Data)
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
	p.video = core.NewReceiver(media, codec)
	p.video.Lossless = p.source.Playback
	p.Medias, p.Receivers = []*core.Media{media}, []*core.Receiver{p.video}
	return nil
}

func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	if p.video == nil || codec != p.video.Codec {
		return nil, core.ErrCantGetTrack
	}
	return p.video, nil
}

func (p *Producer) IsFinite() bool { return p.source.Playback }

func (p *Producer) Stop() error {
	p.cancel()
	if p.source.Playback && p.video != nil {
		// Wake lossless downstream queues before closing their graph nodes.
		p.video.End(context.Canceled)
	}
	return p.Connection.Stop()
}

func (p *Producer) Start() (err error) {
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("exacq: producer already started")
	}
	defer func() {
		p.cancel()
		_ = p.client.Close()
		if errors.Is(err, errPlaybackEnd) {
			err = nil
		}
		if p.source.Playback {
			p.video.End(err)
		}
	}()
	started := time.Now()
	for {
		f, err := p.timed(p.ctx)
		if err != nil {
			return err
		}
		if p.source.Playback {
			elapsed := float64(f.time.Sub(p.ActualStart)) / p.source.Speed
			if elapsed > math.MaxInt64 {
				return errors.New("exacq: playback speed exceeds clock range")
			}
			if err = waitUntil(p.ctx, started.Add(time.Duration(elapsed))); err != nil {
				return err
			}
		}
		if err = p.emit(f, f.duration); err != nil {
			return err
		}
	}
}

// timed returns the next picture with its output timing.
func (p *Producer) timed(ctx context.Context) (*frame, error) {
	for len(p.ready) == 0 {
		if p.ended {
			return nil, errPlaybackEnd
		}
		f, err := p.next(ctx)
		if errors.Is(err, errPlaybackEnd) {
			p.ended = true
			p.ready = p.timeline.flush(p.nominalInterval(), p.source.End)
			continue
		}
		if err != nil {
			return nil, err
		}
		p.ready = p.timeline.push(f)
	}
	f := p.ready[0]
	p.ready[0] = nil
	p.ready = p.ready[1:]
	return f, nil
}

// next returns the next decodable picture in decode order.
func (p *Producer) next(ctx context.Context) (*frame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(p.queued) != 0 {
			f := p.queued[0]
			p.queued[0] = nil
			p.queued = p.queued[1:]
			if err := p.checkAdvance(f); err != nil {
				return nil, err
			}
			return f, nil
		}
		var packet *native.MediaPacket
		var err error
		if p.source.Playback {
			packet, err = p.nextRecorded(ctx)
		} else {
			packet, err = p.client.ReadMedia(ctx)
		}
		if err != nil {
			return nil, err
		}
		// Search verifies its result-specific stream IDs itself. Live stream IDs
		// come from the authenticated configuration and may differ from camera IDs.
		if !p.source.Playback && packet.StreamID != p.stream.WireID {
			continue
		}
		if p.onVideo != nil {
			p.onVideo()
			p.onVideo = nil
		}
		p.Recv += len(packet.Data)
		v, err := p.state.Prepare(packet)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		p.assembled = true
		if p.source.Playback {
			if !v.Time.Before(p.source.End) {
				return nil, errPlaybackEnd
			}
			return newFrame(v), nil
		}
		for _, timed := range p.start.Push(v, p.nominalInterval()) {
			p.queued = append(p.queued, newFrame(timed))
		}
	}
}

// checkAdvance guards live sessions against desynchronized media. B-frame
// streams can step backwards inside a GOP, so only their keyframes must advance.
func (p *Producer) checkAdvance(f *frame) error {
	if f.Key {
		if !p.lastKey.IsZero() && !f.time.After(p.lastKey) {
			return failf(native.CodeProtocolError, native.PhaseLive, "live video keyframe timestamp did not advance")
		}
		p.lastKey = f.time
	}
	if p.timeline != nil && p.timeline.bFrames || f.Picture.B {
		return nil
	}
	if !p.last.IsZero() && !f.time.After(p.last) {
		return failf(native.CodeProtocolError, native.PhaseLive, "live video timestamp did not advance")
	}
	p.last = f.time
	return nil
}

func (p *Producer) nextRecorded(ctx context.Context) (*native.MediaPacket, error) {
	for p.index >= len(p.frames) {
		if !p.cursor.Before(p.source.End) {
			return nil, errPlaybackEnd
		}
		end := p.cursor.Add(native.SearchSpan(p.stream.Format))
		if end.After(p.source.End) {
			end = p.source.End
		}
		frames, err := p.client.Search(ctx, p.stream.ID, p.cursor, end)
		if err != nil {
			return nil, err
		}
		frames = p.dropRepeated(frames)
		p.frames, p.index, p.cursor = frames, 0, end
		if !p.assembled {
			if index := playbackStartIndex(frames, p.source.Start); index > 0 {
				// Whole-second searches can return more than one preceding GOP.
				// Retain their parameter sets without emitting those old pictures.
				for _, frame := range frames[:index] {
					copy := *frame
					copy.Seconds, copy.Milliseconds = 0, 0
					if _, err = p.state.Prepare(&copy); err != nil {
						return nil, err
					}
				}
				p.index = index
			}
		}
	}
	packet := p.frames[p.index]
	p.frames[p.index] = nil
	p.index++
	return packet, nil
}

// dropRepeated removes pictures the previous search already returned:
// adjacent whole-second searches repeat their preceding GOP. Content identity
// is used because B-frame timestamps need not increase in decode order.
func (p *Producer) dropRepeated(frames []*native.MediaPacket) []*native.MediaPacket {
	batch := make(map[[32]byte]struct{}, len(frames))
	filtered := frames[:0]
	for _, frame := range frames {
		hash := sha256.New()
		var identity [7]byte
		binary.LittleEndian.PutUint32(identity[:4], frame.Seconds)
		binary.LittleEndian.PutUint16(identity[4:6], frame.Milliseconds)
		identity[6] = frame.Type
		_, _ = hash.Write(identity[:])
		_, _ = hash.Write(frame.Data)
		var key [32]byte
		copy(key[:], hash.Sum(nil))
		batch[key] = struct{}{}
		if _, seen := p.previousBatch[key]; !seen {
			filtered = append(filtered, frame)
		}
	}
	if len(batch) != 0 {
		p.previousBatch = batch
	}
	return filtered
}

func videoCodec(format uint32, avcc []byte) *core.Codec {
	if format == native.FormatJPEG {
		// Each picture is a complete JPEG, as go2rtc's other JPEG sources produce.
		return &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	}
	if format == 5 {
		return h264.AVCCToCodec(avcc)
	}
	return h265.AVCCToCodec(avcc)
}

// playbackStartIndex is the position of the keyframe native.PlaybackStart picks.
func playbackStartIndex(frames []*native.MediaPacket, start time.Time) int {
	stamp, ok := native.PlaybackStart(frames, start)
	if !ok {
		return -1
	}
	for i, frame := range frames {
		if frame != nil && frame.Type == native.MediaIFrame && frame.Timestamp().Equal(stamp) {
			return i
		}
	}
	return -1
}
