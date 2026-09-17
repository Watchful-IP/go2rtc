package streams

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func TestApiStreamsDeleteConcurrent(t *testing.T) {
	// Each request deletes its own stream: DELETE also patches the config file and
	// returns 400 when the key is already gone, so deleting one name N times can't
	// assert on status. Distinct names still race on the streams map.
	const n = 20
	names := make([]string, n)
	config := "streams:\n"
	for i := range n {
		names[i] = fmt.Sprintf("test%d", i)
		config += "  " + names[i] + ": does_not_matter\n"
	}

	oldConfigPath := app.ConfigPath
	app.ConfigPath = filepath.Join(t.TempDir(), "go2rtc.yaml")
	require.NoError(t, os.WriteFile(app.ConfigPath, []byte(config), 0644))
	t.Cleanup(func() { app.ConfigPath = oldConfigPath })

	streamsMu.Lock()
	for _, name := range names {
		streams[name] = NewStream(nil)
	}
	streamsMu.Unlock()
	t.Cleanup(func() {
		streamsMu.Lock()
		for _, name := range names {
			delete(streams, name)
		}
		streamsMu.Unlock()
	})

	codes := make([]int, n)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("DELETE", "/api/streams?src="+name, nil)
			w := httptest.NewRecorder()
			apiStreams(w, req)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	for i, name := range names {
		require.Equal(t, http.StatusOK, codes[i], name)
		require.Nil(t, Get(name))
	}
}

func TestApiSchemes(t *testing.T) {
	// Setup: Register some test handlers and redirects
	HandleFunc("rtsp", func(url string) (core.Producer, error) { return nil, nil })
	HandleFunc("rtmp", func(url string) (core.Producer, error) { return nil, nil })
	RedirectFunc("http", func(url string) (string, error) { return "", nil })

	t.Run("GET request returns schemes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/schemes", nil)
		w := httptest.NewRecorder()

		apiSchemes(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))

		var schemes []string
		err := json.Unmarshal(w.Body.Bytes(), &schemes)
		require.NoError(t, err)
		require.NotEmpty(t, schemes)

		// Check that our test schemes are in the response
		require.Contains(t, schemes, "rtsp")
		require.Contains(t, schemes, "rtmp")
		require.Contains(t, schemes, "http")
	})
}

func TestApiSchemesNoDuplicates(t *testing.T) {
	// Setup: Register a scheme in both handlers and redirects
	HandleFunc("duplicate", func(url string) (core.Producer, error) { return nil, nil })
	RedirectFunc("duplicate", func(url string) (string, error) { return "", nil })

	req := httptest.NewRequest("GET", "/api/schemes", nil)
	w := httptest.NewRecorder()

	apiSchemes(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var schemes []string
	err := json.Unmarshal(w.Body.Bytes(), &schemes)
	require.NoError(t, err)

	// Count occurrences of "duplicate"
	count := 0
	for _, scheme := range schemes {
		if scheme == "duplicate" {
			count++
		}
	}

	// Should only appear once
	require.Equal(t, 1, count, "scheme 'duplicate' should appear exactly once")
}
