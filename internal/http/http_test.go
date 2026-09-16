package http

import (
	"bytes"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/hls"
	"github.com/stretchr/testify/require"
)

func TestHLSHTTPDispatch(t *testing.T) {
	data, err := os.ReadFile("../../pkg/mp4/testdata/h264.mp4")
	require.NoError(t, err)
	for _, contentType := range []string{"application/x-mpegURL; charset=utf-8", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "secret", r.Header.Get("X-Source-Key"))
				switch r.URL.Path {
				case "/start":
					http.Redirect(w, r, "/nested/INDEX.M3U8", 302)
				case "/nested/INDEX.M3U8":
					w.Header().Set("Content-Type", contentType)
					io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.1,\nseg.m4s\n#EXT-X-ENDLIST")
				case "/nested/init.mp4", "/nested/seg.m4s":
					w.Write(data)
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL+"/start", nil)
			req.Header.Set("X-Source-Key", "secret")
			p, err := do(req)
			require.NoError(t, err)
			defer p.Stop()
			require.IsType(t, &hls.Producer{}, p)
			require.Len(t, p.GetMedias(), 2)
		})
	}
}

func TestProgressiveHTTPDispatch(t *testing.T) {
	data, err := os.ReadFile("../../pkg/mp4/testdata/progressive.mp4")
	require.NoError(t, err)
	for _, contentType := range []string{"video/mp4", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				http.ServeContent(w, r, "clip", time.Time{}, bytes.NewReader(data))
			}))
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL+"/opaque-proxy-token", nil)
			p, err := do(req)
			require.NoError(t, err)
			defer p.Stop()
			require.IsType(t, &mp4.FileProducer{}, p)
		})
	}
}
