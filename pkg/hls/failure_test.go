package hls

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloseCancelsPartialResponseBody(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST")
			return
		}
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(200)
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		close(blocked)
		<-r.Context().Done()
	}))
	defer server.Close()
	reader := openReader(t, server.URL+"/index")
	done := make(chan error, 1)
	go func() { _, _, err := reader.nextSegment(); done <- err }()
	<-blocked
	reader.Close()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("partial body blocked cancellation")
	}
}

func TestHTTPRetriesAreBounded(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST")
			return
		}
		attempts.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	reader := openReader(t, server.URL+"/index")
	_, _, err := reader.nextSegment()
	require.ErrorContains(t, err, "503")
	require.Equal(t, int32(3), attempts.Load())
}

func TestTruncatedSourceDoesNotAffectHealthySource(t *testing.T) {
	init, segment := mp4Fixture(t, "h264")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/init":
			w.Write(init)
		case "/good-segment":
			w.Write(segment)
		case "/bad-segment":
			w.Header().Set("Content-Length", "1000000")
			w.Write(segment[:len(segment)/2])
		case "/bad":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:1.1,\nbad-segment\n#EXT-X-ENDLIST")
		default:
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:1.1,\ngood-segment\n#EXT-X-ENDLIST")
		}
	}))
	defer server.Close()
	res, err := http.Get(server.URL + "/good")
	require.NoError(t, err)
	good, err := OpenResponse(res)
	require.NoError(t, err)
	defer good.Stop()
	done := make(chan error, 1)
	go func() { done <- good.Start() }()
	for i := 0; i < 3; i++ {
		res, err = http.Get(server.URL + "/bad")
		require.NoError(t, err)
		bad, err := OpenResponse(res)
		require.Nil(t, bad)
		require.Error(t, err)
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(3 * time.Second):
		t.Fatal("healthy source stalled")
	}
}
