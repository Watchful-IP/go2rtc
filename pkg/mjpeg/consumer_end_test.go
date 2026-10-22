package mjpeg

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func finiteJPEGTrack(t *testing.T) (*Consumer, *core.Receiver) {
	t.Helper()
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecJPEG, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}}}
	track := core.NewReceiver(media, media.Codecs[0])
	c := NewConsumer()
	require.NoError(t, c.AddTrack(c.Medias[0], media.Codecs[0], track))
	return c, track
}

func writeTo(c *Consumer) (*bytes.Buffer, chan error) {
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := c.WriteTo(&out); done <- err }()
	return &out, done
}

func TestFinitePlaybackEndsTheResponse(t *testing.T) {
	c, track := finiteJPEGTrack(t)
	defer c.Stop()
	out, done := writeTo(c)
	frame := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9}
	track.WriteRTP(&rtp.Packet{Payload: frame})
	track.End(nil)
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(2 * time.Second):
		t.Fatal("MJPEG output stayed open after the finite track ended")
	}
	require.True(t, bytes.Contains(out.Bytes(), frame), "the final picture is written before the response ends")
}

func TestFailedPlaybackPropagatesItsError(t *testing.T) {
	c, track := finiteJPEGTrack(t)
	defer c.Stop()
	_, done := writeTo(c)
	failure := errors.New("recorder closed the connection")
	track.End(failure)
	select {
	case err := <-done:
		require.ErrorIs(t, err, failure)
	case <-time.After(2 * time.Second):
		t.Fatal("MJPEG output stayed open after the track failed")
	}
}

// writes records each Write call separately, like a WebSocket message or a
// multipart part.
type writes struct {
	mu    sync.Mutex
	parts [][]byte
}

func (w *writes) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.parts = append(w.parts, append([]byte(nil), p...))
	return len(p), nil
}

func TestPicturesBeforeOutputKeepTheirBoundaries(t *testing.T) {
	c, track := finiteJPEGTrack(t)
	defer c.Stop()
	frames := [][]byte{{0xff, 0xd8, 1, 0xff, 0xd9}, {0xff, 0xd8, 2, 0xff, 0xd9}, {0xff, 0xd8, 3, 0xff, 0xd9}}
	for _, f := range frames {
		track.WriteRTP(&rtp.Packet{Payload: f})
	}
	track.End(nil)
	time.Sleep(50 * time.Millisecond) // the pictures are queued before the output exists
	out := &writes{}
	done := make(chan error, 1)
	go func() { _, err := c.WriteTo(out); done <- err }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(2 * time.Second):
		t.Fatal("output did not finish")
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	require.Equal(t, frames, out.parts, "each picture is its own write")
}
