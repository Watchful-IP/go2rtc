package mp4

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileHTTPRangeAndHeaders(t *testing.T) {
	data := fixture(t, "progressive")
	var ranges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Source-Key") != "secret" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", `"same"`)
		http.ServeContent(w, r, "clip.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL, nil)
	req.Header.Set("X-Source-Key", "secret")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	p, err := OpenFileResponse(res)
	require.NoError(t, err)
	defer p.Stop()
	require.NoError(t, p.Start())
	require.Positive(t, ranges.Load())
}

func TestFileHTTPSpool(t *testing.T) {
	data := fixture(t, "progressive")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer server.Close()
	res, err := http.Get(server.URL)
	require.NoError(t, err)
	p, err := OpenFileResponse(res)
	require.NoError(t, err)
	r := p.reader.(*fileHTTP)
	require.NotNil(t, r.file)
	require.NoError(t, p.Stop())
	_, statErr := os.Stat(r.file.Name())
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, err = r.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFileHTTPBadRanges(t *testing.T) {
	for _, mode := range []string{"ignored", "wrong-offset", "changed", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("ETag", `"one"`)
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", "5000")
					w.Write(make([]byte, 5000))
					return
				}
				if mode == "ignored" {
					w.Write(make([]byte, 5000))
					return
				}
				contentRange := "bytes 0-4999/5000"
				if mode == "wrong-offset" {
					contentRange = "bytes 1-4999/5000"
				}
				if mode == "changed" {
					w.Header().Set("ETag", `"two"`)
				}
				w.Header().Set("Content-Range", contentRange)
				w.WriteHeader(206)
				if mode == "truncated" {
					w.Write([]byte{0})
					return
				}
				w.Write(make([]byte, 5000))
			}))
			defer server.Close()
			res, err := http.Get(server.URL)
			require.NoError(t, err)
			r, err := newFileHTTP(res)
			require.NoError(t, err)
			defer r.Close()
			_, err = r.ReadAt(make([]byte, 8), 0)
			require.Error(t, err)
		})
	}
}

func TestFileHTTPCancel(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Write(make([]byte, 16))
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	res, err := http.Get(server.URL)
	require.NoError(t, err)
	r, err := newFileHTTP(res)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := r.ReadAt(make([]byte, 8), 0); done <- err }()
	<-started
	r.Close()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("request not cancelled")
	}
}

func TestFiniteBufferBeforeWriter(t *testing.T) {
	c := NewConsumer(nil)
	defer c.Stop()
	// The buffer contract also covers files completing before WriteTo starts.
	_, err := c.wr.Write([]byte("last sample"))
	require.NoError(t, err)
	c.wr.Finish(io.EOF)
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := c.wr.WriteTo(&out); done <- err }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, "last sample", out.String())
	case <-time.After(time.Second):
		t.Fatal("late writer hung")
	}
}
