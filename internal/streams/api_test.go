package streams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func TestApiStreamsDeleteConcurrent(t *testing.T) {
	oldConfigPath := app.ConfigPath
	app.ConfigPath = filepath.Join(t.TempDir(), "go2rtc.yaml")
	t.Cleanup(func() { app.ConfigPath = oldConfigPath })

	streamsMu.Lock()
	streams["test"] = NewStream(nil)
	streamsMu.Unlock()
	t.Cleanup(func() {
		streamsMu.Lock()
		delete(streams, "test")
		streamsMu.Unlock()
	})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("DELETE", "/api/streams?src=test", nil)
			w := httptest.NewRecorder()
			apiStreams(w, req)
			require.Equal(t, http.StatusOK, w.Code)
		}()
	}
	wg.Wait()
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
