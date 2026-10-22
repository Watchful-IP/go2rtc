package exacq

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSnapshotSelectFirstPictureAtOrAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
	}{{"ip.h264", 5}, {"ip.h265", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			start := frames[0].Timestamp().Add(13 * time.Millisecond)
			input, err := selectSnapshot(frames, tc.format, start, start.Add(3*time.Second))
			require.NoError(t, err)
			require.Equal(t, frames[1].Timestamp(), input.capturedAt)
			require.Equal(t, 1, input.index, "preceding keyframe must be decoded but not selected")
			start = frames[15].Timestamp().Add(time.Millisecond)
			input, err = selectSnapshot(frames, tc.format, start, start.Add(3*time.Second))
			require.NoError(t, err)
			require.Equal(t, frames[16].Timestamp(), input.capturedAt)
			require.Equal(t, 1, input.index, "decoder index resets on the preceding keyframe")
			_, err = selectSnapshot(frames, tc.format, frames[29].Timestamp().Add(time.Second), frames[29].Timestamp().Add(4*time.Second))
			require.ErrorIs(t, err, ErrNoSnapshot)
			_, err = selectSnapshot(frames, tc.format, frames[0].Timestamp().Add(-time.Second), frames[0].Timestamp().Add(-time.Millisecond))
			require.ErrorIs(t, err, ErrNoSnapshot)
		})
	}
}

func TestSnapshotLiveUsesBufferedGOP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
	}{{"ip.h264", 5}, {"ip.h265", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			var live []*MediaPacket
			for i, frame := range frames {
				copy := *frame
				if i < 3 {
					copy.Seconds, copy.Milliseconds = 0, 0
				}
				live = append(live, &copy)
			}
			session := &fakeMediaSession{live: live}
			input, err := snapshotSource(context.Background(), session, Stream{ID: 101, WireID: 17, Format: tc.format, Framerate: 15}, nil)
			require.NoError(t, err)
			require.Equal(t, frames[3].Timestamp(), input.capturedAt, "the first timed picture, not the next keyframe")
			require.Equal(t, 3, input.index)
			require.Equal(t, uint32(101), session.selected)
			if path, err := exec.LookPath("ffmpeg"); err == nil {
				result, err := decodeSnapshot(context.Background(), path, input, tc.format, 80)
				require.NoError(t, err)
				var full []byte
				for _, frame := range frames {
					full = append(full, frame.Data...)
				}
				reference, err := decodeSnapshot(context.Background(), path, &snapshotInput{annexB: full, index: 3}, tc.format, 80)
				require.NoError(t, err)
				require.Equal(t, reference.JPEG, result.JPEG)
			}
		})
	}
}

func TestSnapshotLiveRetainsUntimedParameters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
	}{{"ip.h264", 5}, {"ip.h265", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			cached := *frames[0]
			cached.Seconds, cached.Milliseconds = 0, 0
			stripParameters(t, frames[15], tc.format)
			// A discontinuous buffer waits for the next keyframe, which relies
			// on the parameter sets the cached keyframe supplied.
			session := &fakeMediaSession{live: append([]*MediaPacket{&cached}, frames[5:]...)}
			input, err := snapshotSource(context.Background(), session, Stream{ID: 101, WireID: 17, Format: tc.format}, nil)
			require.NoError(t, err)
			require.Equal(t, frames[15].Timestamp(), input.capturedAt)
			require.Zero(t, input.index)
			require.NotEmpty(t, input.annexB)
		})
	}
}

func TestSnapshotHistoryDoesNotFallBackToLive(t *testing.T) {
	frames := fixtureMedia(t, "ip.h264", 5)
	session := &fakeMediaSession{live: frames}
	at := frames[0].Timestamp()
	_, err := snapshotSource(context.Background(), session, Stream{ID: 101, WireID: 17, Format: 5}, &at)
	require.ErrorIs(t, err, ErrNoSnapshot)
	require.Len(t, session.live, 30, "live source must remain unused for missing archive footage")
	require.Equal(t, [][2]time.Time{{at, at.Add(3 * time.Second)}}, session.requests)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = snapshotSource(ctx, session, Stream{ID: 101, WireID: 17, Format: 5}, &at)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSnapshotHistoryWaitsForRecentFootage(t *testing.T) {
	frames := fixtureMedia(t, "ip.h264", 5)
	at := frames[0].Timestamp()
	savedNow, savedPoll := timeNow, edgePollInterval
	defer func() { timeNow, edgePollInterval = savedNow, savedPoll }()
	edgePollInterval = time.Millisecond
	clock := at.Add(2 * time.Second) // webhook arrived before the footage was archived
	timeNow = func() time.Time { clock = clock.Add(time.Second); return clock }

	session := &fakeMediaSession{batches: [][]*MediaPacket{nil, nil, frames}}
	input, err := snapshotSource(context.Background(), session, Stream{ID: 101, WireID: 17, Format: 5}, &at)
	require.NoError(t, err)
	require.NotNil(t, input)
	require.Len(t, session.requests, 3)

	// Past the edge allowance the miss is final.
	clock = at.Add(2 * time.Second)
	session = &fakeMediaSession{}
	_, err = snapshotSource(context.Background(), session, Stream{ID: 101, WireID: 17, Format: 5}, &at)
	require.ErrorIs(t, err, ErrNoSnapshot)
	require.Len(t, session.requests, 14, "polls until 3 s window + 12 s edge allowance has passed")
}

// Decode index -> display index for the generated three-GOP B fixtures.
var bFixtureDisplay = map[string][]int{
	"b.h264":        {0, 3, 1, 2, 6, 4, 5, 9, 7, 8, 12, 10, 11, 14, 13},
	"b-closed.h265": {0, 4, 2, 1, 3, 8, 6, 5, 7, 12, 10, 9, 11, 14, 13},
}

func TestSnapshotReorderedSelectsAnchorByDisplayRank(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format uint32
		after  int // select the first anchor after this decode index
		want   int // decode index of that anchor
	}{
		{"b.h264", 5, 0, 1}, {"b.h264", 5, 1, 4},
		{"b-closed.h265", 6, 0, 1}, {"b-closed.h265", 6, 1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			for _, frame := range frames {
				frame.Type = MediaPFrame // B detection must not rely on the wire label
			}
			start := frames[tc.after].Timestamp().Add(time.Millisecond)
			input, err := selectSnapshot(frames, tc.format, start, start.Add(3*time.Second))
			require.NoError(t, err)
			require.Equal(t, frames[tc.want].Timestamp(), input.capturedAt)
			display := bFixtureDisplay[tc.name]
			rank := 0
			for _, d := range display[:tc.want] {
				if d < display[tc.want] {
					rank++
				}
			}
			require.Equal(t, rank, input.index, "FFmpeg selection is display rank among fed pictures")
			var want []byte
			for _, frame := range frames[:tc.want+1] {
				want = append(want, frame.Data...)
			}
			require.Equal(t, len(want) > 0, len(input.annexB) > 0)
			if path, err := exec.LookPath("ffmpeg"); err == nil {
				result, err := decodeSnapshot(context.Background(), path, input, tc.format, 80)
				require.NoError(t, err)
				var full []byte
				for _, frame := range frames {
					full = append(full, frame.Data...)
				}
				reference, err := decodeSnapshot(context.Background(), path, &snapshotInput{annexB: full, index: display[tc.want]}, tc.format, 80)
				require.NoError(t, err)
				require.Equal(t, reference.JPEG, result.JPEG, "selected JPEG must equal the independently decoded display frame")
			}
		})
	}
}

func TestSnapshotLiveBFrameUsesKeyframe(t *testing.T) {
	frames := fixtureMedia(t, "b.h264", 5)
	cached := *frames[0]
	cached.Seconds, cached.Milliseconds = 0, 0
	session := &fakeMediaSession{live: append([]*MediaPacket{&cached}, frames[1:]...)}
	input, err := snapshotSource(context.Background(), session, Stream{ID: 101, WireID: 17, Format: 5}, nil)
	require.NoError(t, err)
	require.Equal(t, frames[15].Timestamp(), input.capturedAt)
	require.Zero(t, input.index)
}

func TestSnapshotFFmpegDecode(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("optional FFmpeg snapshot decoder unavailable")
	}
	for _, tc := range []struct {
		name   string
		format uint32
	}{{"ip.h264", 5}, {"ip.h265", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			frames := fixtureMedia(t, tc.name, tc.format)
			start := frames[0].Timestamp().Add(13 * time.Millisecond)
			input, err := selectSnapshot(frames, tc.format, start, start.Add(3*time.Second))
			require.NoError(t, err)
			result, err := decodeSnapshot(context.Background(), path, input, tc.format, 80)
			require.NoError(t, err)
			require.Equal(t, frames[1].Timestamp(), result.CapturedAt)
			require.Equal(t, 80, result.Width)
			require.InDelta(t, 45, result.Height, 1, "maintain aspect ratio rounded to an even height")
			decoded, err := jpeg.Decode(bytes.NewReader(result.JPEG))
			require.NoError(t, err)
			require.Equal(t, result.Width, decoded.Bounds().Dx())
			input.index = 0
			key, err := decodeSnapshot(context.Background(), path, input, tc.format, 80)
			require.NoError(t, err)
			require.NotEqual(t, key.JPEG, result.JPEG, "selected image must differ from the preceding keyframe")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = decodeSnapshot(ctx, path, input, tc.format, 80)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestSnapshotBoundedConversion(t *testing.T) {
	for _, invalid := range [][]byte{{0}, {0, 0, 0, 0}, {0, 0, 0, 2, 1}} {
		require.Error(t, appendSnapshotFrame(&snapshotInput{}, invalid))
	}
	output := &snapshotOutput{}
	_, err := output.Write(make([]byte, 8<<20))
	require.NoError(t, err)
	_, err = output.Write([]byte{0})
	require.Error(t, err)
	require.Equal(t, 8<<20, output.Len())
	_, err = decodeSnapshot(context.Background(), "/missing-decoder", &snapshotInput{}, 5, 80)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrNoSnapshot))
}

func TestLabSnapshots(t *testing.T) {
	if os.Getenv("EXACQ_TEST_SNAPSHOTS") != "1" {
		t.Skip("requires coordinated native snapshot lab window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := controlLabConnect(t, ctx)
	options := c.options
	cameras := c.Inventory().Cameras
	_ = c.Close()
	for _, camera := range cameras {
		for _, historical := range []bool{false, true} {
			client, err := Connect(ctx, options)
			require.NoError(t, err)
			var at *time.Time
			if historical {
				stamp := time.Now().Add(-30 * time.Second)
				if raw := os.Getenv("EXACQ_TEST_SNAPSHOT_AT_MS"); raw != "" {
					ms, err := strconv.ParseInt(raw, 10, 64)
					require.NoError(t, err)
					stamp = time.UnixMilli(ms)
				}
				at = &stamp
			}
			started := time.Now()
			result, err := client.Snapshot(ctx, camera.ID, 1280, at)
			require.NoError(t, err, "camera=%d historical=%v", camera.ID, historical)
			require.Equal(t, 1280, result.Width)
			if at != nil {
				require.False(t, result.CapturedAt.Before(*at))
				require.False(t, result.CapturedAt.After(at.Add(3*time.Second)))
			}
			t.Logf("camera=%d historical=%v width=%d height=%d bytes=%d elapsed=%s capturedAt=%s", camera.ID, historical, result.Width, result.Height, len(result.JPEG), time.Since(started).Round(time.Millisecond), result.CapturedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if raw := os.Getenv("EXACQ_TEST_SNAPSHOT_GAP_AT_MS"); raw != "" {
		ms, err := strconv.ParseInt(raw, 10, 64)
		require.NoError(t, err)
		at := time.UnixMilli(ms)
		client, err := Connect(ctx, options)
		require.NoError(t, err)
		started := time.Now()
		_, err = client.Snapshot(ctx, 459264, 1280, &at)
		require.ErrorIs(t, err, ErrNoSnapshot)
		t.Logf("camera=459264 verified empty recording gap at=%s elapsed=%s", at.UTC().Format(time.RFC3339Nano), time.Since(started).Round(time.Millisecond))
	}
}
