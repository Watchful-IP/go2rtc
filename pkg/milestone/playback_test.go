package milestone

import (
	"bytes"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// Recorder times of the playback fixtures (testdata/*_gop*.bin).
const (
	fx264Start  = 1790914879368 // inside fx264_gop0, which starts at 1790914878685
	fx264bStart = 1790914879422
)

func shortEdge(t *testing.T) {
	edgePoll, edgeTimeout = 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { edgePoll, edgeTimeout = 500*time.Millisecond, 10*time.Second })
}

func window(start, end int64, speed string) string {
	q := "&start=" + strconv.FormatInt(start, 10)
	if end != 0 {
		q += "&end=" + strconv.FormatInt(end, 10)
	}
	if speed != "" {
		q += "&speed=" + speed
	}
	return q
}

func TestPlaybackPacesAndEndsAtRange(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.gops = fixtures(t, "fx264", 3)

	prod, err := Dial(rec.url(window(fx264Start, fx264Start+1500, "10")))
	require.NoError(t, err)
	require.True(t, prod.(core.FiniteProducer).IsFinite())

	started := time.Now()
	frames, err := collect(t, prod)
	require.NoError(t, err, "reaching the end of the range is a clean end")
	elapsed := time.Since(started)

	// From the keyframe 683 ms before start to the end of the range, at 10 fps.
	require.Len(t, frames, 22)
	require.True(t, frames[0].key)
	for i, f := range frames {
		require.True(t, f.timed)
		require.InDelta(t, float64(i)*10, f.dts, 0.5, "100 ms pictures at 10x speed")
	}
	require.Greater(t, elapsed, 180*time.Millisecond, "paced: 2.1 s of video at 10x")
	require.Less(t, elapsed, 2*time.Second)

	select {
	case <-prod.(*Playback).video.Done():
	default:
		t.Fatal("track not ended")
	}
}

func TestPlaybackBFramesReordered(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.gops = fixtures(t, "fx264b", 3)

	prod, err := Dial(rec.url(window(fx264bStart, fx264bStart+4000, "20")))
	require.NoError(t, err)
	frames, err := collect(t, prod)
	require.NoError(t, err)
	require.Greater(t, len(frames), 40)
	for i, f := range frames {
		require.GreaterOrEqual(t, f.pts, f.dts, "composition offset at %d", i)
		if i > 0 {
			require.Greater(t, f.dts, frames[i-1].dts)
		}
	}
	for _, step := range presentationSteps(frames) {
		require.InDelta(t, 100.0/20, step, 0.5, "even presentation at 20x")
	}
}

func TestPlaybackEndOfDatabase(t *testing.T) {
	shortEdge(t)
	rec := newFakeRecorder(t)
	rec.gops = fixtures(t, "fx264", 1) // next keeps answering with the newest GOP

	prod, err := Dial(rec.url(window(fx264Start, 0, "20")))
	require.NoError(t, err)
	frames, err := collect(t, prod)
	require.NoError(t, err)
	require.Len(t, frames, 10, "the GOP is not repeated")

	nexts := 0
	for _, m := range rec.methods() {
		if m == "next" {
			nexts++
		}
	}
	require.Greater(t, nexts, pipelineGOPs, "polls the database edge")
	require.Less(t, nexts, 60, "polls at a bounded rate")
}

func TestPlaybackGapIsClosed(t *testing.T) {
	shortEdge(t)
	rec := newFakeRecorder(t)
	gops := fixtures(t, "fx264", 3)
	// A second of missing recording: like the recorder at a sequence end, the
	// first GOP's Next header points across the gap.
	first := bytes.Replace(gops[0], []byte("Next: 1790914879685"), []byte("Next: 1790914880685"), 1)
	require.NotEqual(t, gops[0], first)
	rec.gops = [][]byte{first, gops[2]}

	prod, err := Dial(rec.url(window(fx264Start, fx264Start+3000, "20")))
	require.NoError(t, err)
	frames, err := collect(t, prod)
	require.NoError(t, err)
	require.Len(t, frames, 20)
	for i := 1; i < len(frames); i++ {
		require.InDelta(t, 5, frames[i].dts-frames[i-1].dts, 0.5, "no pause at %d", i)
	}
}

func TestPlaybackStaleFootageBeforeStart(t *testing.T) {
	shortEdge(t)
	rec := newFakeRecorder(t)
	rec.gops = fixtures(t, "fx264", 1)

	// goto into a gap answers with the last GOP before it, a minute earlier.
	_, err := Dial(rec.url(window(fx264Start+60_000, 0, "")))
	require.ErrorIs(t, err, errNoRecording)
}

func TestPlaybackEmptyDatabase(t *testing.T) {
	rec := newFakeRecorder(t)
	_, err := Dial(rec.url(window(fx264Start, 0, "")))
	require.ErrorIs(t, err, errNoRecording)
}

func TestPlaybackFallsBackToRecorderJPEG(t *testing.T) {
	shortEdge(t)
	rec := newFakeRecorder(t)
	rec.gops = [][]byte{firstImage(t, fixture(t, "fxmpeg4_live.bin"))}
	rec.jpegGOPs = fixtures(t, "fxmpeg4_jpeg", 3)

	prod, err := Dial(rec.url(window(1790914879532, 0, "10")))
	require.NoError(t, err)
	require.Equal(t, core.CodecJPEG, prod.GetMedias()[0].Codecs[0].Name)
	require.Equal(t, pipelineJPEG, prod.(*Playback).window, "one picture per request")

	frames, err := collect(t, prod)
	require.NoError(t, err)
	require.Len(t, frames, 3)
	require.True(t, hasMethod(rec.methods(), "goto jpeg"))
}

func TestStopInterruptsPlayback(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.gops = fixtures(t, "fx264", 3)
	prod, err := Dial(rec.url(window(fx264Start, 0, "")))
	require.NoError(t, err)

	done := make(chan error)
	go func() { _, err := collect(t, prod); done <- err }()
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, prod.Stop())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not interrupt pacing")
	}
}

// firstImage returns the first ImageResponse of a captured live stream.
func firstImage(t *testing.T, stream []byte) []byte {
	i := bytes.Index(stream, []byte("ImageResponse\r\n"))
	require.GreaterOrEqual(t, i, 0)
	head := bytes.Index(stream[i:], []byte("\r\n\r\n")) + 4
	m := regexp.MustCompile(`Content-length: (\d+)`).FindSubmatch(stream[i : i+head])
	require.NotNil(t, m)
	n, _ := strconv.Atoi(string(m[1]))
	return stream[i : i+head+n+4]
}

func TestPlaybackBFramesAcrossGap(t *testing.T) {
	shortEdge(t)
	rec := newFakeRecorder(t)
	gops := fixtures(t, "fx264b", 3)
	// The first GOP is the last of a recording sequence: Next is a minute away.
	first := regexp.MustCompile(`Next: \d+`).ReplaceAll(gops[0], []byte("Next: 1790914939789"))
	rec.gops = [][]byte{first, gops[1]}

	prod, err := Dial(rec.url(window(fx264bStart, 0, "20")))
	require.NoError(t, err)
	frames, err := collect(t, prod)
	require.NoError(t, err)
	for _, step := range presentationSteps(frames) {
		require.InDelta(t, 100.0/20, step, 0.5, "keeps 10 fps spacing across the gap")
	}
}

func TestPlaybackMJPEG(t *testing.T) {
	shortEdge(t)
	rec := newFakeRecorder(t)
	rec.gops = fixtures(t, "fxmjpeg", 3)

	prod, err := Dial(rec.url(window(1790914895592, 0, "10")))
	require.NoError(t, err)
	require.Equal(t, core.CodecJPEG, prod.GetMedias()[0].Codecs[0].Name)
	require.Equal(t, pipelineJPEG, prod.(*Playback).window)
	frames, err := collect(t, prod)
	require.NoError(t, err)
	require.Len(t, frames, 3)
	for i := 1; i < len(frames); i++ {
		require.InDelta(t, 200.0/10, frames[i].dts-frames[i-1].dts, 0.5, "5 fps at 10x")
	}
}
