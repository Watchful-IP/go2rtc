package mjpeg

import (
	"io"
	"net/http"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

type Consumer struct {
	core.Connection
	wr        *core.WriteBuffer
	done      chan struct{}
	stopOnce  sync.Once
	started   chan struct{}
	startOnce sync.Once
}

func NewConsumer() *Consumer {
	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionSendonly,
			Codecs: []*core.Codec{
				{Name: core.CodecJPEG},
				{Name: core.CodecRAW},
			},
		},
	}
	wr := core.NewWriteBuffer(nil)
	return &Consumer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "mjpeg",
			Medias:     medias,
			Transport:  wr,
		},
		wr:      wr,
		done:    make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (c *Consumer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, track.Codec)
	sender.Handler = func(packet *rtp.Packet) {
		// Hold pictures until the output exists: writes buffered before it
		// would be joined into one part, and a player shows only the first.
		select {
		case <-c.started:
		case <-c.done:
			return
		}
		if n, err := c.wr.Write(packet.Payload); err == nil {
			c.Send += n
		}
	}

	if track.Codec.IsRTP() {
		sender.Handler = RTPDepay(sender.Handler)
	} else if track.Codec.Name == core.CodecRAW {
		sender.Handler = Encoder(track.Codec, 0, sender.Handler)
	}

	sender.HandleRTP(track)
	c.Senders = append(c.Senders, sender)
	go c.watchEnd(track, sender)
	return nil
}

// watchEnd finishes the output when a finite source (playback) ends, so the
// response closes instead of waiting for frames that never come.
func (c *Consumer) watchEnd(track *core.Receiver, sender *core.Sender) {
	select {
	case <-c.done:
		return
	case <-track.Done():
	}
	// Draining the sender first keeps the final picture.
	sender.Close()
	sender.Wait()
	err := track.EndError()
	if err == nil {
		err = io.EOF
	}
	c.wr.Finish(err)
}

func (c *Consumer) Stop() error {
	c.stopOnce.Do(func() { close(c.done) })
	return c.Connection.Stop()
}

func (c *Consumer) WriteTo(wr io.Writer) (int64, error) {
	out := output{wr}
	c.wr.Reset(out) // install the writer before releasing held pictures
	c.startOnce.Do(func() { close(c.started) })
	return c.wr.WriteTo(out)
}

// output keeps WriteBuffer from treating the destination as its own pre-start
// buffer, while still flushing each picture to HTTP clients.
type output struct{ io.Writer }

func (o output) Flush() {
	if f, ok := o.Writer.(http.Flusher); ok {
		f.Flush()
	}
}
