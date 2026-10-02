package milestone

import (
	"errors"
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/pion/rtp"
)

var errUnsupported = errors.New("milestone: codec needs recorder-side JPEG")

// noVideoTimeout bounds how long a live probe waits while the recorder
// reports the camera as disconnected.
var noVideoTimeout = 10 * time.Second

// Dial opens a live or playback ImageServer session for a milestone:// URL.
func Dial(rawURL string) (core.Producer, error) {
	src, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	if src.Playback {
		return dialPlayback(src)
	}
	return dialLive(src)
}

// stream is the per-session video state shared by live and playback.
type stream struct {
	core.Connection
	src    *Source
	client *Client

	codec    uint16
	video    *core.Receiver
	pictures pictureParser
}

func (s *stream) init(src *Source, client *Client, format string) {
	s.Connection = core.Connection{
		ID:         core.NewID(),
		FormatName: format,
		Protocol:   "tcp",
		RemoteAddr: client.conn.RemoteAddr().String(),
		URL:        src.Redacted(),
		Transport:  client,
	}
	s.src = src
	s.client = client
}

// addTrack builds the video track from the first decodable picture.
func (s *stream) addTrack(f *frame) error {
	var codec *core.Codec
	switch f.codec {
	case codecH264:
		codec = h264.AVCCToCodec(annexb.EncodeToAVCC(f.data))
	case codecH265:
		codec = h265.AVCCToCodec(annexb.EncodeToAVCC(f.data))
		s.pictures.h265 = true
	case codecJPEG:
		codec = &core.Codec{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	default:
		return fmt.Errorf("%w: %s", errUnsupported, codecName(f.codec))
	}
	s.codec = f.codec
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
	s.video = core.NewReceiver(media, codec)
	s.Medias = []*core.Media{media}
	s.Receivers = []*core.Receiver{s.video}
	return nil
}

func (s *stream) payload(f *frame) []byte {
	if f.codec == codecJPEG {
		return f.data
	}
	return annexb.EncodeToAVCC(f.data)
}

// frames parses an image response. Bare JPEG responses carry their time in
// the Current header.
func (s *stream) frames(res *response) ([]frame, error) {
	// Passthrough video cannot carry XProtect privacy masks, so a masked camera
	// falls back to JPEG with the mask rendered by the recorder.
	if mask := res.header["privacymask"]; mask != "" && mask != "none" && !s.client.jpeg {
		return nil, errPrivacyMask
	}
	var out []frame
	var err error
	for _, part := range res.parts {
		if out, err = parseFrames(part, out); err != nil {
			return nil, err
		}
	}
	for i := range out {
		if out[i].time == 0 {
			out[i].time = res.int64("current")
			out[i].sync = out[i].time
		}
	}
	return out, nil
}

var errPrivacyMask = fmt.Errorf("%w: privacy mask", errUnsupported)

// Live streams the recorder's live feed. The recorder starts with its
// pre-buffered GOP, so the first keyframe arrives without waiting.
type Live struct {
	stream
	base  int64
	clock gopClock

	// Pictures are held at the start until the stream is known to use
	// B-frames or not, so the first GOP is never presented out of order.
	held     []heldPicture
	heldAt   time.Time
	decided  bool
	bFrames  bool
	reorder  *reorderer
	firstGOP []frame
}

type heldPicture struct {
	frame
	pic picture
}

// decideTimeout caps how long the first pictures are held. B-frames follow the
// keyframe within a mini-GOP; at low frame rates the second picture decides.
const (
	decideTimeout = 300 * time.Millisecond
	maxHold       = 2 * time.Second
)

func dialLive(src *Source) (*Live, error) {
	jpeg := src.JPEG
	for {
		client, err := dialClient(src, jpeg)
		if err != nil {
			return nil, err
		}
		l := &Live{}
		l.init(src, client, "milestone")
		err = l.probe()
		if err == nil {
			return l, nil
		}
		_ = client.Close()
		if !jpeg && (errors.Is(err, errUnsupported) || errors.Is(err, errVideoBlock)) {
			jpeg = true // the recorder transcodes what browsers cannot decode
			continue
		}
		return nil, err
	}
}

// probe waits for the first keyframe, which defines the track.
func (l *Live) probe() error {
	if err := l.client.Live(); err != nil {
		return err
	}
	deadline := time.Now().Add(noVideoTimeout)
	var connectionLost bool
	for {
		res, err := l.client.read()
		if err != nil {
			if connectionLost {
				return errCameraOffline
			}
			return err
		}
		if res.isXML() {
			if v, ok := liveStatus(res.xml)[statusConnectionLost]; ok {
				connectionLost = v == 1
			}
			if connectionLost && time.Now().After(deadline) {
				return errCameraOffline
			}
			continue
		}
		fs, err := l.frames(res)
		if err != nil {
			return err
		}
		for i, f := range fs {
			if f.key {
				if err = l.addTrack(&f); err != nil {
					return err
				}
				l.base = f.time
				l.firstGOP = fs[i:]
				return nil
			}
		}
	}
}

var errCameraOffline = errors.New("milestone: camera is not connected to the recording server")

func (l *Live) Start() error {
	for _, f := range l.firstGOP {
		if err := l.handle(f); err != nil {
			return err
		}
	}
	l.firstGOP = nil
	for {
		res, err := l.client.read()
		if err != nil {
			return err
		}
		if res.isXML() {
			if liveStatus(res.xml)[statusConnectionLost] == 1 {
				return errCameraOffline
			}
			continue
		}
		fs, err := l.frames(res)
		if err != nil {
			return err
		}
		for _, f := range fs {
			if err = l.handle(f); err != nil {
				return err
			}
		}
	}
}

func (l *Live) handle(f frame) error {
	if f.codec != l.codec {
		// A stream switch or privacy mask; reconnecting re-probes the codec.
		return fmt.Errorf("milestone: codec changed from %s to %s", codecName(l.codec), codecName(f.codec))
	}
	l.Recv += len(f.data)
	f.data = l.payload(&f)
	if l.codec == codecJPEG {
		l.video.Input(&core.Packet{Header: rtp.Header{Timestamp: l.ticks(f.time)}, Payload: f.data})
		return nil
	}

	pic := l.pictures.parse(f.data)
	l.clock.observe(f.time, f.key, !pic.b)
	l.bFrames = l.bFrames || pic.b
	if l.decided {
		l.emit(f, pic)
		return nil
	}
	if l.held == nil {
		l.heldAt = time.Now()
	}
	l.held = append(l.held, heldPicture{f, pic})
	held := time.Since(l.heldAt)
	if l.bFrames {
		// Reordering needs the picture interval; hold until it is known.
		l.decided = l.interval() > 0 || held > maxHold
	} else {
		l.decided = len(l.held) > reorderWindow || held > decideTimeout
	}
	if l.decided {
		for _, h := range l.held {
			l.emit(h.frame, h.pic)
		}
		l.held = nil
	}
	return nil
}

func (l *Live) emit(f frame, pic picture) {
	if l.reorder == nil {
		if !l.bFrames || !f.key {
			// No B-frames: recorder arrival times are presentation times. B-frames
			// that first appear mid-GOP are reordered from the next keyframe.
			l.video.Input(&core.Packet{Header: rtp.Header{Timestamp: l.ticks(f.time)}, Payload: f.data})
			return
		}
		l.reorder = newReorderer(reorderWindow)
	}
	interval := l.interval()
	if interval == 0 {
		interval = 40 // 25 fps when nothing better is known
	}
	if f.key {
		for _, t := range l.reorder.flush(interval) {
			l.writeTimed(t)
		}
		l.reorder.rebase(float64(f.time-l.base), interval)
	}
	poc := pic.poc
	if !pic.ok {
		poc = int(f.time - l.base) // keep decode order when the header is unreadable
	}
	for _, t := range l.reorder.push(f, poc, interval) {
		l.writeTimed(t)
	}
}

func (l *Live) interval() float64 {
	return l.clock.get(l.pictures.interval)
}

func (l *Live) ticks(t int64) uint32 {
	return uint32(max(t-l.base, 0) * clockRate)
}

func (l *Live) writeTimed(t timedFrame) {
	packet := &core.Packet{Header: rtp.Header{Timestamp: uint32(t.pts * clockRate)}, Payload: t.data}
	core.SetSampleTiming(packet, core.SampleTiming{
		ClockID:           l.ID,
		DecodeTime:        uint64(t.dts * clockRate),
		Duration:          uint32(t.duration * clockRate),
		CompositionOffset: uint32((t.pts - t.dts) * clockRate),
	})
	l.video.Input(packet)
}
