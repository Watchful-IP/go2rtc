package streams

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceHost(t *testing.T) {
	for _, tc := range []struct{ source, proto, host string }{
		{"rtsps://rtsp.cloud.yoursix.com:322/live/cam1?token=SECRET", "rtsps", "rtsp.cloud.yoursix.com:322"},
		{"rtsp://admin:password@10.0.0.5:554/Streaming/Channels/101", "rtsp", "10.0.0.5:554"},
		{"rtsp://admin:pa/ss@word@cam.local/live", "rtsp", "cam.local"},
		{"ffmpeg:rtsp://u:p@cam.local/x#video=copy#audio=copy", "rtsp", "cam.local"},
		{"http://cam.local/snap.jpg?user=admin&pwd=secret", "http", "cam.local"},
		{"rtsp://[fe80::1]:554/live#backchannel=0", "rtsp", "[fe80::1]:554"},
		{"exec:ffmpeg -i rtsp://a:b@h/p -c copy", "rtsp", "h"},
		{"ffmpeg:camera1#video=h264", "ffmpeg", ""},
	} {
		proto, host := sourceHost(tc.source)
		require.Equal(t, tc.proto, proto, tc.source)
		require.Equal(t, tc.host, host, tc.source)
	}
}
