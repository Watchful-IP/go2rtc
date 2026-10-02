package milestone

import (
	"bytes"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/stretchr/testify/require"
)

type captured struct {
	pts, dts float64 // ms
	timed    bool
	key      bool
	payload  []byte
}

// collect starts a producer and records what its video receiver emits until
// Start returns.
func collect(t *testing.T, prod core.Producer) ([]captured, error) {
	var out []captured
	medias := prod.GetMedias()
	require.Len(t, medias, 1)
	r, err := prod.GetTrack(medias[0], medias[0].Codecs[0])
	require.NoError(t, err)
	r.Input = func(packet *core.Packet) {
		c := captured{pts: float64(packet.Timestamp) / clockRate, payload: packet.Payload}
		if timing, ok := core.GetSampleTiming(packet); ok {
			c.timed = true
			c.dts = float64(timing.DecodeTime) / clockRate
		} else {
			c.dts = c.pts
		}
		switch r.Codec.Name {
		case core.CodecH264:
			c.key = h264.IsKeyframe(packet.Payload)
		case core.CodecJPEG:
			c.key = true
		}
		out = append(out, c)
	}
	err = prod.Start()
	return out, err
}

func presentationSteps(frames []captured) []float64 {
	pts := make([]float64, len(frames))
	for i, f := range frames {
		pts[i] = f.pts
	}
	sort.Float64s(pts)
	steps := make([]float64, len(pts)-1)
	for i := range steps {
		steps[i] = pts[i+1] - pts[i]
	}
	return steps
}

func TestLiveH264(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fx264_live.bin")

	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	codec := prod.GetMedias()[0].Codecs[0]
	require.Equal(t, core.CodecH264, codec.Name)
	require.Contains(t, codec.FmtpLine, "sprop-parameter-sets=")

	frames, err := collect(t, prod)
	require.Error(t, err) // the fixture ends and the recorder hangs up
	require.Len(t, frames, 25)
	require.True(t, frames[0].key, "starts at the recorder's pre-buffered keyframe")
	for i, f := range frames {
		require.False(t, f.timed, "no B-frames: recorder times pass through")
		if i > 0 {
			require.Greater(t, f.pts, frames[i-1].pts)
		}
	}
	for _, step := range presentationSteps(frames) {
		require.InDelta(t, 100, step, 3, "10 fps camera")
	}
}

func TestLiveBFramesPresentInOrder(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fx264b_live.bin")

	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	frames, err := collect(t, prod)
	require.Error(t, err)
	// The last pictures stay in the reorder window when the recorder hangs up.
	require.GreaterOrEqual(t, len(frames), 30-reorderWindow)

	var decodeOrder []float64
	for i, f := range frames {
		require.True(t, f.timed, "B-frame streams carry explicit decode times")
		require.GreaterOrEqual(t, f.pts, f.dts, "composition offset at %d", i)
		decodeOrder = append(decodeOrder, f.dts)
	}
	require.True(t, sort.Float64sAreSorted(decodeOrder))
	reordered := false
	for i := 1; i < len(frames); i++ {
		reordered = reordered || frames[i].pts < frames[i-1].pts
	}
	require.True(t, reordered, "presentation order differs from decode order")
	for _, step := range presentationSteps(frames) {
		require.InDelta(t, 100, step, 5, "evenly spaced at 10 fps despite burst arrival")
	}
}

func TestLiveMJPEG(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fxmjpeg_live.bin")

	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	require.Equal(t, core.CodecJPEG, prod.GetMedias()[0].Codecs[0].Name)
	frames, _ := collect(t, prod)
	require.Len(t, frames, 8)
	for _, f := range frames {
		require.Equal(t, []byte{0xFF, 0xD8}, f.payload[:2])
	}
}

func TestLiveFallsBackToRecorderJPEG(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fxmpeg4_live.bin") // MPEG-4 Part 2 cannot be played by browsers
	rec.jpegLive = fixture(t, "fxmpeg4_jpeg_live.bin")

	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	require.Equal(t, core.CodecJPEG, prod.GetMedias()[0].Codecs[0].Name)
	require.Equal(t, []string{"connect", "live", "connect jpeg", "live jpeg"}, rec.methods())

	frames, _ := collect(t, prod)
	require.Len(t, frames, 4)
	require.Equal(t, []byte{0xFF, 0xD8}, frames[0].payload[:2])
}

func TestLiveJPEGOnRequest(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.jpegLive = fixture(t, "fxmpeg4_jpeg_live.bin")

	_, err := Dial(rec.url("&jpeg=1"))
	require.NoError(t, err)
	require.Equal(t, []string{"connect jpeg", "live jpeg"}, rec.methods())
}

func TestLiveCameraOffline(t *testing.T) {
	noVideoTimeout = 0
	t.Cleanup(func() { noVideoTimeout = 10 * time.Second })

	rec := newFakeRecorder(t)
	rec.live = []byte(statusLost + statusLost)
	_, err := Dial(rec.url(""))
	require.ErrorIs(t, err, errCameraOffline)
}

func TestConnectRejected(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.connect = `<?xml version="1.0" encoding="UTF-8"?><methodresponse><requestid>1</requestid><methodname>connect</methodname><connected>no</connected><errorreason>Unknown device</errorreason></methodresponse>` + "\r\n\r\n"
	_, err := Dial(rec.url(""))
	require.EqualError(t, err, "milestone: connect: Unknown device")
}

func TestParseURL(t *testing.T) {
	src, err := ParseURL("milestonex://recorder/" + testCamera + "?token=TOKEN%23a%23b%2F%2FServerConnector%23&stream=28DC44C3-079E-4C94-8EC9-60363451EB40&start=1790914879368&end=2026-10-02T05:01:29Z&speed=4")
	require.NoError(t, err)
	require.Equal(t, "recorder:7563", src.URL.Host)
	require.Equal(t, "TOKEN#a#b//ServerConnector#", src.Token)
	require.True(t, src.Playback)
	require.Equal(t, int64(1790914879368), src.Start.UnixMilli())
	require.Equal(t, 4.0, src.Speed)
	require.NotContains(t, src.Redacted(), "ServerConnector")

	for _, bad := range []string{
		"milestone://recorder/not-a-guid?token=x",
		"milestone://recorder/" + testCamera,
		"milestone://recorder/" + testCamera + "?token=x&end=1",
		"milestone://recorder/" + testCamera + "?token=x&start=2&end=1",
		"milestone://recorder/" + testCamera + "?token=x&start=1&speed=0",
		"rtsp://recorder/" + testCamera + "?token=x",
	} {
		_, err = ParseURL(bad)
		require.Error(t, err, bad)
	}
}

func TestProducerReportsRedactedURL(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fx264_live.bin")
	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	url := prod.(*Live).URL
	require.True(t, strings.Contains(url, "token=xxx"), url)
	_ = prod.Stop()
}

func TestStopUnblocksLive(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fx264_live.bin")
	rec.holdOpen = true
	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	done := make(chan error)
	go func() { _, err := collect(t, prod); done <- err }()
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, prod.Stop())
	select {
	case err = <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not unblock Start")
	}
}

func TestLivePrivacyMaskFallsBackToRecorderJPEG(t *testing.T) {
	rec := newFakeRecorder(t)
	live := fixture(t, "fx264_live.bin")
	rec.live = bytes.Replace(live, []byte("PrivacyMask: none"), []byte("PrivacyMask: grid;4;4;0110"), 1)
	require.NotEqual(t, live, rec.live)
	rec.jpegLive = fixture(t, "fxmpeg4_jpeg_live.bin")

	prod, err := Dial(rec.url(""))
	require.NoError(t, err)
	require.Equal(t, core.CodecJPEG, prod.GetMedias()[0].Codecs[0].Name)
	require.Equal(t, []string{"connect", "live", "connect jpeg", "live jpeg"}, rec.methods())
}
