package mp4

import (
	"bytes"
	"errors"
	"io"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

type Consumer struct {
	core.Connection
	wr           *core.WriteBuffer
	muxer        *Muxer
	mu           sync.Mutex
	start        bool
	pending      []pendingSample
	pendingBytes int
	done         chan struct{}
	started      chan struct{}
	startOnce    sync.Once
	stopOnce     sync.Once
	ended        int
	endErr       error
	hasVideo     bool
	clockSet     bool
	clockID      uint32

	Rotate int `json:"-"`
	ScaleX int `json:"-"`
	ScaleY int `json:"-"`
}

func NewConsumer(medias []*core.Media) *Consumer {
	if medias == nil {
		// default local medias
		medias = []*core.Media{
			{
				Kind:      core.KindVideo,
				Direction: core.DirectionSendonly,
				Codecs: []*core.Codec{
					{Name: core.CodecH264},
					{Name: core.CodecH265},
				},
			},
			{
				Kind:      core.KindAudio,
				Direction: core.DirectionSendonly,
				Codecs: []*core.Codec{
					{Name: core.CodecAAC},
				},
			},
		}
	}

	wr := core.NewWriteBuffer(nil)
	return &Consumer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "mp4",
			Medias:     medias,
			Transport:  wr,
		},
		done:    make(chan struct{}),
		started: make(chan struct{}),
		muxer:   &Muxer{},
		wr:      wr,
	}
}

func (c *Consumer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	trackID := byte(len(c.Senders))
	if track.Codec.IsVideo() {
		c.mu.Lock()
		c.hasVideo = true
		c.mu.Unlock()
	}

	codec := track.Codec.Clone()
	handler := core.NewSender(media, codec)

	switch track.Codec.Name {
	case core.CodecH264:
		handler.Handler = func(packet *rtp.Packet) { c.writePacket(trackID, packet, true, h264.IsKeyframe(packet.Payload)) }

		if track.Codec.IsRTP() {
			handler.Handler = h264.RTPDepay(track.Codec, handler.Handler)
		} else {
			handler.Handler = h264.RepairAVCC(track.Codec, handler.Handler)
		}

	case core.CodecH265:
		handler.Handler = func(packet *rtp.Packet) { c.writePacket(trackID, packet, true, h265.IsKeyframe(packet.Payload)) }

		if track.Codec.IsRTP() {
			handler.Handler = h265.RTPDepay(track.Codec, handler.Handler)
		} else {
			handler.Handler = h265.RepairAVCC(track.Codec, handler.Handler)
		}

	default:
		handler.Handler = func(packet *rtp.Packet) { c.writePacket(trackID, packet, false, false) }

		switch track.Codec.Name {
		case core.CodecAAC:
			if track.Codec.IsRTP() {
				handler.Handler = aac.RTPDepay(handler.Handler)
			}
		case core.CodecOpus, core.CodecMP3: // no changes
		case core.CodecPCMA, core.CodecPCMU, core.CodecPCM, core.CodecPCML:
			codec.Name = core.CodecFLAC
			if codec.Channels == 2 {
				// hacky way for support two channels audio
				codec.Channels = 1
				codec.ClockRate *= 2
			}
			handler.Handler = pcm.FLACEncoder(track.Codec.Name, codec.ClockRate, handler.Handler)

		default:
			handler.Handler = nil
		}
	}

	if handler.Handler == nil {
		s := "mp4: unsupported codec: " + track.Codec.String()
		println(s)
		return errors.New(s)
	}

	c.muxer.AddTrack(codec)

	handler.HandleRTP(track)
	c.Senders = append(c.Senders, handler)
	go c.watchEnd(track, handler)

	return nil
}

func (c *Consumer) WriteTo(wr io.Writer) (int64, error) {
	c.startOnce.Do(func() { close(c.started) })
	if len(c.Senders) == 1 && c.Senders[0].Codec.IsAudio() {
		c.mu.Lock()
		c.start = true
		c.flushPending()
		c.mu.Unlock()
	}

	init, err := c.muxer.GetInit()
	if err != nil {
		return 0, err
	}

	if c.Rotate != 0 {
		PatchVideoRotate(init, c.Rotate)
	}
	if c.ScaleX != 0 && c.ScaleY != 0 {
		PatchVideoScale(init, c.ScaleX, c.ScaleY)
	}

	if _, err = wr.Write(init); err != nil {
		return 0, err
	}

	return c.wr.WriteTo(wr)
}

type pendingSample struct {
	trackID byte
	packet  *rtp.Packet
}

func (c *Consumer) writePacket(trackID byte, packet *rtp.Packet, video, keyframe bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timing, ok := core.GetSampleTiming(packet); ok {
		if c.clockSet && timing.ClockID != c.clockID {
			if int32(timing.ClockID-c.clockID) < 0 {
				return
			}
			c.start = !c.hasVideo
			c.pending = nil
			c.pendingBytes = 0
		}
		c.clockSet = true
		c.clockID = timing.ClockID
	}
	if !c.start {
		if !video {
			// Track senders run independently; retain a small, bounded audio lead-in
			// so scheduling cannot discard audio aligned with the first keyframe.
			if _, timed := core.GetSampleTiming(packet); timed && len(c.pending) < 128 && c.pendingBytes+len(packet.Payload) <= 64<<10 {
				clone := *packet
				clone.Payload = bytes.Clone(packet.Payload)
				c.pending = append(c.pending, pendingSample{trackID, &clone})
				c.pendingBytes += len(clone.Payload)
			}
			return
		}
		if !keyframe {
			return
		}
		c.start = true
	}
	c.writeSample(trackID, packet)
	c.flushPending()
}

func (c *Consumer) writeSample(trackID byte, packet *rtp.Packet) {
	b := c.muxer.GetPayload(trackID, packet)
	if n, err := c.wr.Write(b); err == nil {
		c.Send += n
	}
}

func (c *Consumer) flushPending() {
	for _, sample := range c.pending {
		c.writeSample(sample.trackID, sample.packet)
	}
	c.pending = nil
	c.pendingBytes = 0
}
