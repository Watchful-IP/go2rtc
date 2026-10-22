package exacq

import (
	"context"
	"errors"
	"testing"
	"time"

	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
	"github.com/stretchr/testify/require"
)

func TestSourceSelectLowest(t *testing.T) {
	source, err := ParseURL("exacq://user:password@recorder/101?select=lowest")
	require.NoError(t, err)
	require.True(t, source.SelectLowest)
	for _, raw := range []string{
		"exacq://user:password@recorder/101?select=lowest&stream=7",
		"exacq://user:password@recorder/101?select=highest",
		"exacq://user:password@recorder/101?reorder=gop&gop=15",
	} {
		_, err = ParseURL(raw)
		var typed *native.Error
		require.True(t, errors.As(err, &typed), raw)
		require.Equal(t, native.CodeInvalidRequest, typed.Code, raw)
	}
}

func TestLowestStream(t *testing.T) {
	inventory := &native.Inventory{Streams: []native.Stream{
		{ID: 101, ParentCameraID: 101, Format: 5, Width: 1920, Height: 1080, Enabled: true},
		{ID: 102, ParentCameraID: 101, Format: 6, Width: 640, Height: 360, Enabled: true},
		{ID: 103, ParentCameraID: 101, Format: 5, Width: 320, Height: 180, Enabled: false},
		{ID: 104, ParentCameraID: 101, Format: 0, Width: 160, Height: 90, Enabled: true},
		{ID: 201, ParentCameraID: 201, Format: 0, Width: 640, Height: 360, Enabled: true},
		{ID: 301, ParentCameraID: 301, Format: 5, Width: 640, Height: 360, Enabled: false},
		{ID: 401, ParentCameraID: 401, Format: 3, Width: 640, Height: 360, Enabled: true},
	}}
	streams, err := streamCandidates(inventory, &Source{Camera: 101, SelectLowest: true})
	stream := streams[0]
	require.NoError(t, err)
	require.Equal(t, uint32(102), stream.ID, "smallest enabled H.264/H.265 stream, preferred over a smaller JPEG")
	streams, err = streamCandidates(inventory, &Source{Camera: 201, SelectLowest: true})
	require.NoError(t, err)
	require.Equal(t, native.FormatJPEG, streams[0].Format, "a JPEG-only camera plays JPEG")
	for camera, code := range map[uint32]native.Code{401: native.CodeUnsupportedCodec, 301: native.CodeStreamUnavailable, 999: native.CodeCameraNotFound} {
		_, err = streamCandidates(inventory, &Source{Camera: camera, SelectLowest: true})
		var typed *native.Error
		require.True(t, errors.As(err, &typed))
		require.Equal(t, code, typed.Code, "camera %d", camera)
	}
}

func TestSilentStreamsAreTriedLast(t *testing.T) {
	streams := []native.Stream{{ID: 1}, {ID: 2}, {ID: 3}}
	markSilent("serial-a", 1)
	ordered := preferResponsive("serial-a", streams)
	require.Equal(t, []uint32{2, 3, 1}, []uint32{ordered[0].ID, ordered[1].ID, ordered[2].ID})
	ordered = preferResponsive("serial-b", streams)
	require.Equal(t, uint32(1), ordered[0].ID, "memory is per recorder")
}

// pacedSession delivers live packets at a fixed pace, or none at all.
type pacedSession struct {
	fakeMediaSession
	every time.Duration
}

func (s *pacedSession) ReadMedia(ctx context.Context) (*native.MediaPacket, error) {
	if len(s.live) == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.every):
	}
	return s.fakeMediaSession.ReadMedia(ctx)
}

func TestSilenceMeansNoVideoNotNoKeyframeYet(t *testing.T) {
	saved := silentStreamTimeout
	silentStreamTimeout = 40 * time.Millisecond
	defer func() { silentStreamTimeout = saved }()
	source, err := ParseURL("exacq://user:password@recorder/101?select=lowest")
	require.NoError(t, err)
	inventory := &native.Inventory{Streams: []native.Stream{{ID: 101, ParentCameraID: 101, WireID: 17, Format: 5, Framerate: 15, Enabled: true}}}

	// Video flows from mid-GOP: the next keyframe lands well after the
	// silence timeout, and the stream must still be accepted.
	frames := fixtureMedia(t, "ip.h264", 5)[1:]
	flowing := &pacedSession{fakeMediaSession: fakeMediaSession{inventory: inventory, live: frames}, every: 10 * time.Millisecond}
	p, err := dialStream(context.Background(), source, flowing, inventory.Streams[0], true)
	require.NoError(t, err)
	_ = p.Stop()

	// Nothing arrives at all: that is a silent stream.
	quiet := &pacedSession{fakeMediaSession: fakeMediaSession{inventory: inventory}}
	_, err = dialStream(context.Background(), source, quiet, inventory.Streams[0], true)
	require.ErrorIs(t, err, errSilentStream)
}

func TestPlaybackSearchIsNotCutShortBySilenceFallback(t *testing.T) {
	saved := silentStreamTimeout
	silentStreamTimeout = 20 * time.Millisecond
	defer func() { silentStreamTimeout = saved }()
	source, err := ParseURL("exacq://user:password@recorder/101?select=lowest&start=1700000000000&end=1700000010000")
	require.NoError(t, err)
	inventory := &native.Inventory{Streams: []native.Stream{
		{ID: 101, ParentCameraID: 101, WireID: 17, Format: 5, Framerate: 15, Enabled: true, Width: 640, Height: 360},
		{ID: 102, ParentCameraID: 101, WireID: 18, Format: 5, Framerate: 15, Enabled: true, Width: 1920, Height: 1080},
	}}
	session := &slowSearchSession{fakeMediaSession: fakeMediaSession{inventory: inventory, batches: [][]*native.MediaPacket{fixtureMedia(t, "ip.h264", 5)}}, delay: 100 * time.Millisecond}
	p, err := dialStream(context.Background(), source, session, inventory.Streams[0], true)
	require.NoError(t, err)
	_ = p.Stop()
}

// slowSearchSession answers a recorded search only after a delay, as a real
// search does once its result is complete.
type slowSearchSession struct {
	fakeMediaSession
	delay time.Duration
}

func (s *slowSearchSession) Search(ctx context.Context, id uint32, start, end time.Time) ([]*native.MediaPacket, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
	}
	return s.fakeMediaSession.Search(ctx, id, start, end)
}
