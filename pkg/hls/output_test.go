package hls

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/stretchr/testify/require"
)

type captureOutput struct {
	sync.Mutex
	data  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *captureOutput) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	n, err := w.data.Write(p)
	w.once.Do(func() { close(w.ready) })
	return n, err
}

func TestNativeHLSConsumerTiming(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	for _, name := range []string{"h264", "h265"} {
		t.Run(name, func(t *testing.T) {
			init, segment := mp4Fixture(t, name)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/init":
					w.Write(init)
				case "/seg":
					w.Write(segment)
				default:
					io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:1.1,\nseg\n#EXT-X-ENDLIST")
				}
			}))
			defer server.Close()
			res, err := http.Get(server.URL + "/index")
			require.NoError(t, err)
			producer, err := OpenResponse(res)
			require.NoError(t, err)
			defer producer.Stop()
			consumer := mp4.NewConsumer(nil)
			defer consumer.Stop()
			for _, m := range producer.GetMedias() {
				codec := m.Codecs[0]
				track, err := producer.GetTrack(m, codec)
				require.NoError(t, err)
				require.NoError(t, consumer.AddTrack(m, codec, track))
			}
			output := &captureOutput{ready: make(chan struct{})}
			written := make(chan struct{})
			go func() { defer close(written); _, _ = consumer.WriteTo(output) }()
			<-output.ready
			require.ErrorIs(t, producer.Start(), io.EOF)
			// Drain asynchronous senders before closing the writer.
			for _, s := range consumer.Senders {
				s.Close()
				s.Wait()
			}
			require.NoError(t, consumer.Stop())
			<-written
			path := filepath.Join(t.TempDir(), "output.mp4")
			require.NoError(t, os.WriteFile(path, output.data.Bytes(), 0600))
			comparePacketTiming(t, ffprobe, "../mp4/testdata/"+name+".mp4", path)
		})
	}
}

func comparePacketTiming(t *testing.T, ffprobe, source, output string) {
	t.Helper()
	type packet struct {
		Stream   int    `json:"stream_index"`
		PTS      string `json:"pts_time"`
		DTS      string `json:"dts_time"`
		Duration string `json:"duration_time"`
		Hash     string `json:"data_hash"`
	}
	read := func(path string) map[int][]packet {
		cmd := exec.Command(ffprobe, "-v", "error", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=stream_index,pts_time,dts_time,duration_time,data_hash", "-of", "json", path)
		b, err := cmd.Output()
		require.NoError(t, err)
		var v struct{ Packets []packet }
		require.NoError(t, json.Unmarshal(b, &v))
		tracks := map[int][]packet{}
		for _, p := range v.Packets {
			tracks[p.Stream] = append(tracks[p.Stream], p)
		}
		return tracks
	}
	want, got := read(source), read(output)
	require.Len(t, got, len(want))
	seconds := func(v string) float64 { x, err := strconv.ParseFloat(v, 64); require.NoError(t, err); return x }
	for id, expected := range want {
		require.Len(t, got[id], len(expected), "track %d sample count", id)
		for i, p := range expected {
			actual := got[id][i]
			// One codec-clock tick plus ffprobe's six-digit decimal formatting.
			tolerance := 1.0/90000 + 0.000001
			if id == 1 {
				tolerance = 1.0/48000 + 0.000001
			}
			require.InDelta(t, seconds(p.PTS), seconds(actual.PTS), tolerance, "track %d sample %d PTS", id, i)
			require.InDelta(t, seconds(p.DTS), seconds(actual.DTS), tolerance, "track %d sample %d DTS", id, i)
			require.InDelta(t, seconds(p.Duration), seconds(actual.Duration), tolerance, "track %d sample %d duration", id, i)
			if id == 1 {
				require.Equal(t, p.Hash, actual.Hash, "audio payload %d", i)
			}
		}
	}
}
