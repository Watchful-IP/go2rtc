package hls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/stretchr/testify/require"
)

// Private vendor fixtures stay outside version control. Only counts are logged.
func TestExternalFixtures(t *testing.T) {
	root := os.Getenv("GO2RTC_HLS_FIXTURES")
	if root == "" {
		t.Skip("set GO2RTC_HLS_FIXTURES to a private fixture directory")
	}
	for _, resolution := range []string{"low_res", "high_res"} {
		t.Run(resolution, func(t *testing.T) {
			init, err := os.ReadFile(filepath.Join(root, resolution, "init.mp4"))
			require.NoError(t, err)
			d := &mp4.Demuxer{}
			medias, err := d.Probe(init)
			require.NoError(t, err)
			require.Len(t, medias, 2)
			paths, err := filepath.Glob(filepath.Join(root, resolution, "segment*.m4s"))
			require.NoError(t, err)
			require.NotEmpty(t, paths)
			for _, path := range paths {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				samples, err := d.Demux(data)
				require.NoError(t, err)
				joined := append(append([]byte{}, init...), data...)
				file := filepath.Join(t.TempDir(), "source.mp4")
				require.NoError(t, os.WriteFile(file, joined, 0600))
				cmd := exec.Command("ffprobe", "-v", "error", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=stream_index,dts,pts,data_hash", "-of", "json", file)
				b, err := cmd.Output()
				require.NoError(t, err)
				var oracle struct {
					Packets []struct {
						StreamIndex int `json:"stream_index"`
						DTS, PTS    uint64
						Hash        string `json:"data_hash"`
					}
				}
				require.NoError(t, json.Unmarshal(b, &oracle))
				require.Len(t, samples, len(oracle.Packets))
				byID := make(map[uint32][]mp4.Sample)
				for _, s := range samples {
					byID[s.TrackID] = append(byID[s.TrackID], s)
				}
				counts := make(map[uint32]int)
				for id, v := range byID {
					counts[id] = len(v)
				}
				for _, expected := range oracle.Packets {
					id := uint32(expected.StreamIndex + 1)
					require.NotEmpty(t, byID[id])
					s := byID[id][0]
					byID[id] = byID[id][1:]
					require.Equal(t, expected.DTS, s.DecodeTime)
					clock := uint64(medias[expected.StreamIndex].Codecs[0].ClockRate)
					scale := uint64(s.TimeScale)
					require.Equal(t, uint32(expected.PTS/scale)*uint32(clock)+uint32(expected.PTS%scale*clock/scale), s.Packet.Timestamp)
					hash := sha256.Sum256(s.Packet.Payload)
					require.Equal(t, strings.ToLower(expected.Hash), "sha256:"+hex.EncodeToString(hash[:]))
				}
				t.Logf("%s: verified %d packets, tracks %v", filepath.Base(path), len(samples), counts)
			}
		})
	}
}

func TestExternalLive(t *testing.T) {
	script := os.Getenv("GO2RTC_HLS_LIVE_SCRIPT")
	if script == "" {
		t.Skip("set GO2RTC_HLS_LIVE_SCRIPT to a URL-generating helper")
	}
	var raw []byte
	for _, resolution := range []string{"low_res", "high_res"} {
		t.Run(resolution, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if raw == nil {
				helper := exec.CommandContext(ctx, script, resolution)
				helper.WaitDelay = 2 * time.Second
				candidate, err := helper.Output()
				if err != nil {
					t.Fatal("stream URL helper failed")
				}
				raw = candidate
			}
			u, err := url.Parse(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal("invalid stream URL")
			}
			query := u.Query()
			query.Set("resolution", resolution)
			u.RawQuery = query.Encode()

			req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			if err != nil {
				t.Fatal("invalid stream URL")
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal("initial HTTP request failed")
			}
			if res.StatusCode != 200 {
				res.Body.Close()
				t.Fatalf("initial HTTP status %d", res.StatusCode)
			}
			prod, err := OpenResponse(res)
			require.NoError(t, err)
			defer prod.Stop()
			require.Len(t, prod.GetMedias(), 2)
			counts := make(map[string]int)
			for _, m := range prod.GetMedias() {
				codec := m.Codecs[0]
				r, err := prod.GetTrack(m, codec)
				require.NoError(t, err)
				r.Input = func(p *core.Packet) { counts[codec.Name]++ }
			}
			timer := time.AfterFunc(12*time.Second, func() { prod.Stop() })
			defer timer.Stop()
			err = prod.Start()
			require.ErrorIs(t, err, context.Canceled)
			for codec, count := range counts {
				require.Greater(t, count, 20)
				t.Logf("%s: %s received %d packets", resolution, codec, count)
			}
			require.Len(t, counts, 2)
		})
	}
}
