package hls

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openReader(t *testing.T, rawURL string) *reader {
	t.Helper()
	res, err := http.Get(rawURL)
	require.NoError(t, err)
	rd, err := newReader(res.Request, res.Body)
	require.NoError(t, err)
	t.Cleanup(func() { rd.Close() })
	return rd
}

func TestReaderSequenceRangesAndEnd(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:2@0\nall.ts\n#EXTINF:1,\n#EXT-X-BYTERANGE:2\nall.ts\n#EXT-X-ENDLIST")
			return
		}
		requests.Add(1)
		switch r.Header.Get("Range") {
		case "bytes=0-1":
			w.Header().Set("Content-Range", "bytes 0-1/4")
			w.WriteHeader(206)
			io.WriteString(w, "ab")
		case "bytes=2-3":
			w.Header().Set("Content-Range", "bytes 2-3/4")
			w.WriteHeader(206)
			io.WriteString(w, "cd")
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	rd := openReader(t, server.URL+"/index.m3u8")
	b, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "abcd", string(b))
	require.Equal(t, int32(2), requests.Load())
}

func TestReaderRedirectHeadersAndRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "secret", r.Header.Get("X-Source-Key"))
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/nested/master.m3u8", 302)
		case "/nested/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\nmedia/index.m3u8")
		case "/nested/media/index.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST")
		case "/nested/media/segment.ts":
			if attempts.Add(1) < 2 {
				w.WriteHeader(503)
			} else {
				io.WriteString(w, "ok")
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/start", nil)
	req.Header.Set("X-Source-Key", "secret")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	rd, err := newReader(res.Request, res.Body)
	require.NoError(t, err)
	defer rd.Close()
	b, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "ok", string(b))
	require.Equal(t, int32(2), attempts.Load())
}

func TestReaderCloseCancelsDownload(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nseg.ts")
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	rd := openReader(t, server.URL+"/index.m3u8")
	done := make(chan error, 1)
	go func() { _, _, err := rd.nextSegment(); done <- err }()
	<-entered
	require.NoError(t, rd.Close())
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel HTTP request")
	}
}

func TestReaderLiveSequence(t *testing.T) {
	var reloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			n := reloads.Add(1)
			if n < 3 {
				io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:0.01\n#EXT-X-MEDIA-SEQUENCE:5\n#EXTINF:0.01,\nsame.ts")
			} else {
				io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:6\n#EXTINF:0.01,\nsame.ts\n#EXT-X-ENDLIST")
			}
			return
		}
		io.WriteString(w, "a")
	}))
	defer server.Close()
	rd := openReader(t, server.URL+"/index.m3u8")
	b, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "aa", string(b))
	require.Equal(t, int32(3), reloads.Load())
}

func TestReaderDoesNotLeakHeadersAcrossOrigins(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("X-Source-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		io.WriteString(w, "ok")
	}))
	defer sink.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, sink.URL+"/segment", 302)
			return
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1,\n%s/segment\n#EXTINF:1,\n/redirect\n#EXT-X-ENDLIST", sink.URL)
	}))
	defer source.Close()
	req, _ := http.NewRequest("GET", source.URL+"/index", nil)
	req.Header.Set("X-Source-Key", "secret")
	req.Header.Set("Authorization", "Bearer secret")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	rd, err := newReader(res.Request, res.Body)
	require.NoError(t, err)
	defer rd.Close()
	b, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "okok", string(b))
}

func TestReaderLimitsAndErrors(t *testing.T) {
	_, err := readLimited(strings.NewReader("12345"), 4)
	require.Error(t, err)
	for _, status := range []int{200, 404, 416} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/index" {
					io.WriteString(w, "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:2@0\nseg.ts\n#EXT-X-ENDLIST")
					return
				}
				w.WriteHeader(status)
				io.WriteString(w, "aa")
			}))
			defer server.Close()
			rd := openReader(t, server.URL+"/index")
			_, _, err := rd.nextSegment()
			require.Error(t, err)
		})
	}
}
