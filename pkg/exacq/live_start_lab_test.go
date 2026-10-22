package exacq

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/reorder"
)

// TestLabLiveStart records what the recorder sends before the first timed
// keyframe and whether that cached keyframe decodes with what follows.
// Read-only: one live session per camera, no configuration writes.
func TestLabLiveStart(t *testing.T) {
	if os.Getenv("EXACQ_TEST_LIVE_START") == "" {
		t.Skip("set EXACQ_TEST_LIVE_START with the lab address and credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inventory := controlLabConnect(t, ctx).Inventory()
	dir := t.TempDir()
	for _, camera := range inventory.Cameras {
		stream, err := selectStream(inventory, &Source{Camera: camera.ID, SelectLowest: true})
		if err != nil {
			t.Logf("camera %d: %v", camera.ID, err)
			continue
		}
		t.Logf("camera %d %q: stream %d format %d %dx%d enabled=%v connected=%v", camera.ID, camera.Name, stream.ID, stream.Format, stream.Width, stream.Height, stream.Enabled, stream.Connected != nil && *stream.Connected)
		c := controlLabConnect(t, ctx)
		started := time.Now()
		if err = c.StartLive(stream.ID); err != nil {
			t.Fatal(err)
		}
		pictures := reorder.Parser{H265: stream.Format == 6}
		var all, timed bytes.Buffer
		var firstAny, firstTimed time.Duration = -1, -1
		count := 0
		for time.Since(started) < 5*time.Second {
			packet, err := c.ReadMedia(ctx)
			if err != nil {
				t.Logf("camera %d: %v after %d pictures", camera.ID, err, count)
				break
			}
			if packet.StreamID != stream.WireID || packet.Type == 0 {
				continue
			}
			arrival := time.Since(started)
			stamp := packet.Timestamp()
			key := packet.Type == MediaIFrame
			if firstAny < 0 && key {
				firstAny = arrival
			}
			if firstAny < 0 {
				continue
			}
			all.Write(packet.Data)
			if firstTimed < 0 && key && !stamp.IsZero() {
				firstTimed = arrival
			}
			if firstTimed >= 0 {
				timed.Write(packet.Data)
			}
			pic := pictures.Parse(annexBToAVCC(packet.Data))
			if count < 40 {
				age := "untimed"
				if !stamp.IsZero() {
					age = time.Since(stamp).Round(time.Millisecond).String()
				}
				t.Logf("camera %d #%d at %v type=%d age=%s poc=%d b=%v", camera.ID, count, arrival.Round(time.Millisecond), packet.Type, age, pic.POC, pic.B)
			}
			count++
		}
		_ = c.Close()
		codec := "h264"
		if stream.Format == 6 {
			codec = "hevc"
		}
		for name, data := range map[string][]byte{"cached": all.Bytes(), "timed": timed.Bytes()} {
			path := filepath.Join(dir, name)
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			out, _ := exec.Command("ffmpeg", "-v", "error", "-f", codec, "-i", path, "-f", "null", "-").CombinedOutput()
			frames, _ := exec.Command("ffprobe", "-v", "error", "-f", codec, "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path).Output()
			t.Logf("camera %d %s %dx%d %s: first keyframe %v, first timed keyframe %v, decoded %s frames, decoder errors: %q",
				camera.ID, codec, stream.Width, stream.Height, name, firstAny.Round(time.Millisecond), firstTimed.Round(time.Millisecond),
				strings.TrimSpace(string(frames)), strings.TrimSpace(string(out)))
		}
	}
}

func annexBToAVCC(data []byte) []byte {
	nalus, err := splitAnnexB(data)
	if err != nil {
		return nil
	}
	var avcc []byte
	for _, nalu := range nalus {
		avcc = appendNALU(avcc, nalu)
	}
	return avcc
}

// TestLabLiveSnapshotStart times live snapshots per camera (read-only).
func TestLabLiveSnapshotStart(t *testing.T) {
	if os.Getenv("EXACQ_TEST_LIVE_START") == "" {
		t.Skip("set EXACQ_TEST_LIVE_START with the lab address and credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := controlLabConnect(t, ctx)
	options, cameras := c.options, c.Inventory().Cameras
	_ = c.Close()
	for _, camera := range cameras {
		started := time.Now()
		client, err := Connect(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Snapshot(ctx, camera.ID, 1280, nil)
		if err != nil {
			t.Logf("camera %d: %v after %v", camera.ID, err, time.Since(started).Round(time.Millisecond))
			continue
		}
		t.Logf("camera %d: %dx%d %d bytes in %v, age %v", camera.ID, result.Width, result.Height, len(result.JPEG),
			time.Since(started).Round(time.Millisecond), time.Since(result.CapturedAt).Round(time.Millisecond))
	}
}

// TestLabSilentStreamFallback dials select=lowest twice in one process: the
// second dial skips streams the first found silent.
func TestLabSilentStreamFallback(t *testing.T) {
	if os.Getenv("EXACQ_TEST_LIVE_START") == "" {
		t.Skip("set EXACQ_TEST_LIVE_START with the lab address and credentials")
	}
	u := producerLabURL(t)
	u.RawQuery = "select=lowest"
	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		started := time.Now()
		producer, err := DialContext(ctx, u.String())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		p := producer.(*Producer)
		t.Logf("attempt %d: stream %d %dx%d after %v", attempt, p.stream.ID, p.stream.Width, p.stream.Height, time.Since(started).Round(time.Millisecond))
		_ = p.Stop()
	}
}
