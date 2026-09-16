package hls

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlaylist(t *testing.T) {
	base, _ := url.Parse("https://host/path/index.m3u8?token=playlist-only")
	p, err := parsePlaylist([]byte("#EXTM3U\r\n#EXT-X-MEDIA-SEQUENCE:9007199254740993\r\n#EXT-X-MAP:URI=\"init,one.mp4?token=init\",BYTERANGE=\"20@3\"\r\n#EXTINF:1,\r\n#EXT-X-BYTERANGE:4@23\r\nall.mp4\r\n#EXTINF:1,\r\n#EXT-X-BYTERANGE:4\r\nall.mp4\r\n#EXT-X-DISCONTINUITY\r\n#EXT-X-MAP:URI=\"/other.mp4\"\r\n#EXTINF:1,\r\nlast.m4s\r\n#EXT-X-ENDLIST"), base)
	require.NoError(t, err)
	require.Len(t, p.segments, 3)
	require.Equal(t, uint64(9007199254740993), p.segments[0].sequence)
	require.Equal(t, "https://host/path/init,one.mp4?token=init", p.segments[0].init.uri)
	require.Equal(t, int64(27), p.segments[1].offset)
	require.Equal(t, "https://host/path/all.mp4", p.segments[1].uri)
	require.Equal(t, uint64(1), p.segments[2].discontinuity)
	require.True(t, p.end)
}

func TestPlaylistRejectsAmbiguity(t *testing.T) {
	base, _ := url.Parse("https://host/index.m3u8")
	for _, body := range []string{
		"not a playlist", "#EXTM3U\n#EXTINF:NaN,\nx", "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:4\nx",
		"#EXTM3U\n#EXT-X-MAP:URI=\"x\",URI=\"y\"", "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key\"",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXT-X-MAP:URI=\"init.mp4\"",
		"#EXTM3U\n#EXTINF:1,\nfile:///tmp/test", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,URI=\"audio.m3u8\"",
	} {
		_, err := parsePlaylist([]byte(body), base)
		require.Error(t, err, body)
	}
}

func FuzzPlaylist(f *testing.F) {
	f.Add([]byte("#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1,\nx.m4s\n#EXT-X-ENDLIST"))
	f.Add([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\",IV=0x1"))
	base, _ := url.Parse("https://host/index.m3u8")
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxPlaylistSize {
			return
		}
		_, _ = parsePlaylist(data, base)
	})
}
