package exacq

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
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
	client, err := Connect(ctx, source.options())
	if err != nil {
		return nil, err
	}
	candidates, err := streamCandidates(client.Inventory(), source)
	if err == nil {
		candidates = preferResponsive(client.discovery.Serial, candidates)
		err = checkLicense(ctx, client)
	}
	for i := 0; err == nil; i++ {
		var p *Producer
		p, err = dialStream(ctx, source, client, candidates[i], i < len(candidates)-1)
		if err == nil {
			return p, nil
		}
		_ = client.Close()
		if !errors.Is(err, errSilentStream) {
			return nil, err
		}
		markSilent(client.discovery.Serial, candidates[i].ID)
		if i == len(candidates)-1 {
			return nil, err
		}
		// A configured, enabled stream can still send nothing (an unconnected
		// substream). Interrupting its read closed the session; try the next.
		if client, err = Connect(ctx, source.options()); err != nil {
			return nil, err
		}
	}
	_ = client.Close()
	return nil, err
}

// silentStreamTimeout bounds waiting on a stream with fallbacks left. A live
// stream answers within milliseconds with the recorder's buffered GOP.
const silentStreamTimeout = 4 * time.Second

var errSilentStream = errors.New("exacq: live stream sent no video")

func dialStream(ctx context.Context, source *Source, client *Client, stream Stream, fallback bool) (*Producer, error) {
	probeCtx, cancel := ctx, context.CancelFunc(func() {})
	if fallback {
		probeCtx, cancel = context.WithTimeout(ctx, silentStreamTimeout)
	}
	defer cancel()
	p := newProducer(source, client, stream, client.conn.RemoteAddr().String())
	if err := p.probe(probeCtx); err != nil {
		p.cancel()
		return nil, err
	}
	return p, nil
}

// checkLicense fails before requesting media, so a stream reports the reason
// rather than a timeout.
func checkLicense(ctx context.Context, client *Client) error {
	license, err := client.ReadLicense(ctx)
	if err != nil {
		return err
	}
	return licenseUsable(license)
}

type mediaSession interface {
	Inventory() *Inventory
	StartLive(uint32) error
	ReadMedia(context.Context) (*MediaPacket, error)
	Search(context.Context, uint32, time.Time, time.Time) ([]*MediaPacket, error)
	Close() error
}

type Producer struct {
	core.Connection
	// ActualStart is the timestamp of the first decodable picture. Playback
	// can start before the requested range to retain necessary GOP preroll.
	ActualStart time.Time `json:"actual_start"`

	source   *Source
	client   mediaSession
	stream   Stream
	video    *core.Receiver
	state    videoState
	timeline *timeline
	start    liveStart
	queued   []*videoFrame // live pictures released by start, decode order
	ready    []*videoFrame
	ended    bool
	ctx      context.Context
	cancel   context.CancelFunc
	started  atomic.Bool
	clockID  uint32
	seq      uint16
	lastKey  time.Time
	last     time.Time

	cursor        time.Time
	frames        []*MediaPacket
	index         int
	previousBatch map[[32]byte]struct{}
}

func newProducer(source *Source, client mediaSession, stream Stream, address string) *Producer {
	p := &Producer{source: source, client: client, stream: stream, state: videoState{format: stream.Format, keepUntimed: !source.Playback}, cursor: source.Start, clockID: core.NewID()}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	format := "exacq"
	if source.Playback {
		format += "/playback"
	}
	p.Connection = core.Connection{ID: core.NewID(), FormatName: format, Protocol: "tcp", RemoteAddr: address, URL: source.Redacted(), Transport: client}
	return p
}

func selectStream(inventory *Inventory, source *Source) (Stream, error) {
	if inventory == nil {
		return Stream{}, errorf(CodeInternal, PhaseLive, "missing recorder inventory")
	}
	if source.SelectLowest {
		return lowestStream(inventory, source.Camera)
	}
	id := source.Camera
	if source.Stream != 0 {
		id = source.Stream
	}
	for _, stream := range inventory.Streams {
		if stream.ID != id {
			continue
		}
		if source.Stream != 0 && stream.ParentCameraID != source.Camera && stream.ID != source.Camera {
			return Stream{}, errorf(CodeCameraNotFound, PhaseLive, "stream %d does not belong to camera %d", id, source.Camera)
		}
		if !stream.Enabled {
			return Stream{}, errorf(CodeStreamUnavailable, PhaseLive, "stream %d of camera %d is disabled on the recorder", stream.ID, source.Camera)
		}
		if stream.Format != 5 && stream.Format != 6 {
			return Stream{}, errorf(CodeUnsupportedCodec, PhaseLive, "stream %d uses an unsupported video format (%s); select an H.264 or H.265 stream", stream.ID, formatName(stream.Format))
		}
		return stream, nil
	}
	if source.Stream != 0 {
		return Stream{}, errorf(CodeCameraNotFound, PhaseLive, "stream %d of camera %d is not on this recorder", id, source.Camera)
	}
	return Stream{}, errorf(CodeCameraNotFound, PhaseLive, "camera %d is not on this recorder", id)
}

// streamCandidates is the stream to play, or for select=lowest every enabled
// H.264/H.265 stream of the camera, smallest first.
func streamCandidates(inventory *Inventory, source *Source) ([]Stream, error) {
	if !source.SelectLowest {
		stream, err := selectStream(inventory, source)
		return []Stream{stream}, err
	}
	return lowestStreams(inventory, source.Camera)
}

func lowestStream(inventory *Inventory, camera uint32) (Stream, error) {
	streams, err := lowestStreams(inventory, camera)
	if err != nil {
		return Stream{}, err
	}
	return streams[0], nil
}

func lowestStreams(inventory *Inventory, camera uint32) ([]Stream, error) {
	var streams []Stream
	found, enabled := false, 0
	var formats []string
	for _, stream := range inventory.Streams {
		if stream.ID != camera && stream.ParentCameraID != camera {
			continue
		}
		found = true
		if !stream.Enabled {
			continue
		}
		enabled++
		if stream.Format != 5 && stream.Format != 6 {
			formats = append(formats, formatName(stream.Format))
			continue
		}
		streams = append(streams, stream)
	}
	sort.SliceStable(streams, func(i, j int) bool {
		return uint64(streams[i].Width)*uint64(streams[i].Height) < uint64(streams[j].Width)*uint64(streams[j].Height)
	})
	switch {
	case len(streams) != 0:
		return streams, nil
	case !found:
		return nil, errorf(CodeCameraNotFound, PhaseLive, "camera %d is not on this recorder", camera)
	case enabled == 0:
		return nil, errorf(CodeStreamUnavailable, PhaseLive, "camera %d has no enabled stream on the recorder", camera)
	}
	return nil, errorf(CodeUnsupportedCodec, PhaseLive, "camera %d has no enabled H.264 or H.265 stream (found %s)", camera, strings.Join(formats, ", "))
}

// probe waits for the first decodable keyframe, which defines the track.
func (p *Producer) probe(ctx context.Context) error {
	if !p.source.Playback {
		if err := p.client.StartLive(p.stream.ID); err != nil {
			return err
		}
	}
	f, err := p.next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errorf(CodeNoRecording, PhaseSearch, "no decodable recording in the requested range")
		}
		var typed *Error
		if !p.source.Playback && (errors.Is(err, context.DeadlineExceeded) || errors.As(err, &typed) && typed.Code == CodeTimeout) {
			return wrapf(errors.Join(errSilentStream, err), CodeStreamUnavailable, PhaseLive, "camera %d sent no video on stream %d (%dx%d); it may be offline or disconnected from the recorder", p.source.Camera, p.stream.ID, p.stream.Width, p.stream.Height)
		}
		return err
	}
	p.ActualStart = f.time
	signalled := 0.0
	if rate := p.stream.Framerate; rate > 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0) {
		signalled = 1000 / rate
	}
	p.timeline = newTimeline(f.time, p.source.Playback, signalled, &p.state.pictures)
	p.ready = p.timeline.push(f)
	codec := p.state.codec(f.data)
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
		if errors.Is(err, io.EOF) {
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
func (p *Producer) timed(ctx context.Context) (*videoFrame, error) {
	for len(p.ready) == 0 {
		if p.ended {
			return nil, io.EOF
		}
		f, err := p.next(ctx)
		if errors.Is(err, io.EOF) {
			p.ended = true
			p.ready = p.timeline.flush(p.endDuration())
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

// endDuration keeps the last playback picture inside the requested end.
func (p *Producer) endDuration() time.Duration {
	duration := p.nominalInterval()
	if pending := p.timeline.pending; pending != nil && p.source.Playback {
		if remaining := p.source.End.Sub(pending.time); remaining > 0 && remaining < duration {
			duration = remaining
		}
	}
	return duration
}

// next returns the next decodable picture in decode order.
func (p *Producer) next(ctx context.Context) (*videoFrame, error) {
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
		var packet *MediaPacket
		var err error
		if p.source.Playback {
			packet, err = p.nextRecorded(ctx)
		} else {
			packet, err = p.client.ReadMedia(ctx)
		}
		if err != nil {
			return nil, err
		}
		// Search verifies its result-specific stream IDs itself. Live stream
		// IDs come from the authenticated configuration and may differ from CASID.
		if !p.source.Playback && packet.StreamID != p.stream.WireID {
			continue
		}
		p.Recv += len(packet.Data)
		f, err := p.state.prepare(packet)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		if p.source.Playback {
			if !f.time.Before(p.source.End) {
				return nil, io.EOF
			}
			return f, nil
		}
		p.queued = p.start.push(f, p.nominalInterval())
	}
}

// checkAdvance guards live sessions against desynchronized media. B-frame
// streams can step backwards inside a GOP (HEVC on 26.1), so only their
// keyframes must advance.
func (p *Producer) checkAdvance(f *videoFrame) error {
	if f.key {
		if !p.lastKey.IsZero() && !f.time.After(p.lastKey) {
			return errorf(CodeProtocolError, PhaseLive, "live video keyframe timestamp did not advance")
		}
		p.lastKey = f.time
	}
	if p.timeline != nil && p.timeline.bFrames || f.pic.B {
		return nil
	}
	if !p.last.IsZero() && !f.time.After(p.last) {
		return errorf(CodeProtocolError, PhaseLive, "live video timestamp did not advance")
	}
	p.last = f.time
	return nil
}

func (p *Producer) nextRecorded(ctx context.Context) (*MediaPacket, error) {
	for p.index >= len(p.frames) {
		if !p.cursor.Before(p.source.End) {
			return nil, io.EOF
		}
		end := p.cursor.Add(10 * time.Second)
		if end.After(p.source.End) {
			end = p.source.End
		}
		frames, err := p.client.Search(ctx, p.stream.ID, p.cursor, end)
		if err != nil {
			return nil, err
		}
		frames = p.dropRepeated(frames)
		p.frames, p.index, p.cursor = frames, 0, end
		if !p.state.ready {
			if index := playbackStartIndex(frames, p.source.Start); index > 0 {
				// Whole-second searches can return more than one preceding GOP.
				// Retain their parameter sets without emitting those old pictures.
				for _, frame := range frames[:index] {
					copy := *frame
					copy.Seconds, copy.Milliseconds = 0, 0
					if _, err = p.state.prepare(&copy); err != nil {
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
func (p *Producer) dropRepeated(frames []*MediaPacket) []*MediaPacket {
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

func formatName(format uint32) string {
	switch format {
	case 0:
		return "JPEG"
	case 5:
		return "H.264"
	case 6:
		return "H.265"
	}
	return fmt.Sprintf("format %d", format)
}
