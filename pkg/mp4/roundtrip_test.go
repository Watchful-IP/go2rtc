package mp4

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Optional end-to-end decoder check; the packet oracle tests need no external binary.
func TestFFmpegRoundTrip(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	for _, name := range []string{"h264", "h265"} {
		t.Run(name, func(t *testing.T) {
			d := &Demuxer{}
			data := fixture(t, name)
			medias, err := d.Probe(data)
			require.NoError(t, err)
			m := &Muxer{}
			ids := map[uint32]byte{}
			for i, media := range medias {
				codec := media.Codecs[0]
				m.AddTrack(codec)
				ids[d.GetTrackID(codec)] = byte(i)
			}
			init, err := m.GetInit()
			require.NoError(t, err)
			out := bytes.NewBuffer(init)
			samples, err := d.Demux(data)
			require.NoError(t, err)
			for _, s := range samples {
				out.Write(m.GetPayload(ids[s.TrackID], s.Packet))
			}
			path := filepath.Join(t.TempDir(), "remux.mp4")
			require.NoError(t, os.WriteFile(path, out.Bytes(), 0600))
			frameHashes := func(path string) []string {
				cmd := exec.Command(ffmpeg, "-v", "error", "-i", path, "-map", "0:v:0", "-fps_mode", "passthrough", "-f", "framemd5", "-")
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				b, err := cmd.Output()
				require.NoError(t, err, stderr.String())
				require.Empty(t, stderr.String())
				var hashes []string
				for _, line := range strings.Split(string(b), "\n") {
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					fields := strings.Split(line, ",")
					hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
				}
				return hashes
			}
			expected := frameHashes("testdata/" + name + ".mp4")
			require.Len(t, expected, 11)
			require.Equal(t, expected, frameHashes(path))
		})
	}
}
