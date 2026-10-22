package streams

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/yaml"
	"github.com/stretchr/testify/require"
)

func publicNamesFixture(t *testing.T) {
	t.Helper()
	oldStreams := streams
	oldPublic, oldPrivate := publicNames, privateNames
	publicNames, privateNames = map[string]string{}, map[string]string{}
	streams = map[string]*Stream{}
	oldPath := app.ConfigPath
	app.ConfigPath = filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(app.ConfigPath, nil, 0600))
	oldHandlers := handlers
	handlers = map[string]Handler{}
	for _, scheme := range []string{"exacq", "exacqsdk", "exacqsdks", "rtsp"} {
		HandleFunc(scheme, func(string) (core.Producer, error) { panic("must not dial") })
	}
	t.Cleanup(func() {
		streams, handlers, app.ConfigPath = oldStreams, oldHandlers, oldPath
		publicNames, privateNames = oldPublic, oldPrivate
	})
}

func streamsRequest(method string, query url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	apiStreams(w, httptest.NewRequest(method, "/api/streams?"+query.Encode(), nil))
	return w
}

func TestExacqPublicNamesDoNotExposeSecrets(t *testing.T) {
	publicNamesFixture(t)
	raw := []string{
		"exacq://admin:recorderSecret@recorder/1?proxy=socks5%3A%2F%2FproxyUser%3AproxySecret%40gateway",
		"exacq://admin:anotherSecret@recorder/1?proxy=socks5%3A%2F%2FproxyUser%3AproxySecret%40gateway",
		"exacqsdk://gateway/internal/stream/bearerSecret",
		"exacqsdks://gateway/internal/stream/anotherBearerSecret",
	}
	for _, src := range raw {
		_, err := GetOrPatch(url.Values{"src": {src}})
		require.NoError(t, err)
	}
	// Existing config entries and PUT use the same projection as unnamed sources.
	configured := "exacq://admin:configuredSecret@recorder/2"
	streams[configured] = NewStream(configured)
	put := "exacq://admin:putSecret@recorder/3"
	require.Equal(t, 200, streamsRequest("PUT", url.Values{"src": {put}}).Code)
	raw = append(raw, configured, put)
	body := streamsRequest("GET", nil).Body.Bytes()
	for _, secret := range []string{"admin", "recorderSecret", "anotherSecret", "proxyUser", "proxySecret", "bearerSecret", "anotherBearerSecret", "configuredSecret", "putSecret"} {
		require.NotContains(t, string(body), secret)
	}
	var listed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &listed))
	require.Len(t, listed, len(raw))
	require.Equal(t, string(body), streamsRequest("GET", nil).Body.String())
	for _, src := range raw {
		alias := publicNameLocked(src)
		require.Contains(t, listed, alias)
		require.Same(t, Get(src), Get(alias))
		require.Contains(t, GetAllNames(), alias)
		require.Contains(t, GetAllSources(), alias)
		require.Equal(t, 200, streamsRequest("GET", url.Values{"src": {alias}}).Code)
		linked, err := Patch("named", alias)
		require.NoError(t, err)
		require.Same(t, Get(src), linked)
		Delete("named")
	}
}

type publicNameConnection struct{ core.Connection }

func (*publicNameConnection) Start() error { return nil }

func TestExacqPublicAliasActionsAndConfigIdentity(t *testing.T) {
	publicNamesFixture(t)
	raw := "exacq://admin:secret@recorder/1"
	require.Equal(t, 200, streamsRequest("PUT", url.Values{"src": {raw}}).Code)
	alias := publicNameLocked(raw)
	source := "exacq://admin:replacement@recorder/2"
	require.Equal(t, 200, streamsRequest("PUT", url.Values{"name": {alias}, "src": {source}}).Code)
	require.Same(t, Get(raw), Get(alias))
	require.Equal(t, []string{source}, Get(raw).Sources())
	data, err := os.ReadFile(app.ConfigPath)
	require.NoError(t, err)
	var config struct {
		Streams map[string]any `yaml:"streams"`
	}
	require.NoError(t, yaml.Unmarshal(data, &config))
	require.Contains(t, config.Streams, raw)
	require.NotContains(t, config.Streams, alias)
	require.Equal(t, 200, streamsRequest("PATCH", url.Values{"name": {alias}, "src": {raw}}).Code)
	stream := Get(raw)
	stream.producers[0].conn = &publicNameConnection{core.Connection{ID: 42, FormatName: "alias-selection"}}
	for _, selection := range []string{raw, alias} {
		w := httptest.NewRecorder()
		apiStreamsDOT(w, httptest.NewRequest("GET", "/api/streams.dot?"+url.Values{"src": {selection}}.Encode(), nil))
		require.Contains(t, w.Body.String(), "alias-selection")
	}
	require.Equal(t, 200, streamsRequest("DELETE", url.Values{"src": {alias}}).Code)
	require.Nil(t, Get(alias))
	require.Nil(t, Get(raw))
	require.Empty(t, GetAll())
	data, err = os.ReadFile(app.ConfigPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), raw)
	require.Equal(t, 404, streamsRequest("GET", url.Values{"src": {alias}}).Code)
}

func TestExacqPublicNamesPreserveOrdinaryNamesAndAvoidCollisions(t *testing.T) {
	publicNamesFixture(t)
	raw := "exacq://admin:secret@recorder/1"
	stream := NewStream(raw)
	streams[raw] = stream
	collision := publicNameLocked(raw)
	forgetPublicNameLocked(raw)
	streams[collision] = NewStream(nil) // A pre-existing explicit config name.
	alias := publicNameLocked(raw)
	require.NotEqual(t, collision, alias)
	require.Same(t, stream, Get(alias))
	require.Same(t, streams[collision], Get(collision))
	for _, name := range []string{"office", "rtsp://user:password@host/1"} {
		streams[name] = NewStream(nil)
		require.Contains(t, GetAll(), name)
	}
	require.Len(t, GetAll(), 4)
	Delete(collision)
	require.Equal(t, alias, publicNameLocked(raw))
	streams[collision] = NewStream(nil)
	Delete(alias)
	require.Nil(t, Get(alias))
	require.NotNil(t, Get(collision))
}
