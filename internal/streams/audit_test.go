package streams

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"rtsp://admin:password@10.0.0.5:554/Streaming/Channels/101", "rtsp://10.0.0.5:554/Streaming/Channels/101"},
		{"rtsps://rtsp.cloud.yoursix.com:322/live/cam1?token=SECRET", "rtsps://rtsp.cloud.yoursix.com:322/live/cam1"},
		{"ffmpeg:rtsp://u:p@cam.local/x?auth=1#video=copy#audio=copy", "ffmpeg:rtsp://cam.local/x"},
		{"rtsp://user:p@ss@cam.local/live", "rtsp://cam.local/live"},
		{"http://cam.local/snap.jpg?user=admin&pwd=secret", "http://cam.local/snap.jpg"},
		{"rtsp://[fe80::1]:554/live#backchannel=0", "rtsp://[fe80::1]:554/live"},
		{"exec:ffmpeg -i rtsp://a:b@h/p -c copy", "exec:ffmpeg -i rtsp://h/p -c copy"},
		{`streams: dial rtsps://u:p@h:322/x?token=T: i/o timeout`, `streams: dial rtsps://h:322/x: i/o timeout`},
		{`Get "http://u:p@h/a?b=c": EOF`, `Get "http://h/a": EOF`},
		{"t-1a2b3c4d-0123456789abcdef", "t-1a2b3c4d-0123456789abcdef"},
		{"dial tcp 10.0.0.5:554: connect: connection refused", "dial tcp 10.0.0.5:554: connect: connection refused"},
	} {
		require.Equal(t, tc.want, RedactURL(tc.in), tc.in)
	}
}

func TestParseAuditMeta(t *testing.T) {
	query, err := url.ParseQuery("name=s&src=rtsp://h/x" +
		"&meta.tenant=0f8fad5b-d9cb-469f-a165-70867728950e" +
		"&meta.camera=" + url.QueryEscape("vp/0f8fad5b-d9cb-469f-a165-70867728950e/Cam%2B1") +
		"&meta.bad=has%20space" +
		"&meta.inject=a%0aINF%20fake" +
		"&meta.=empty" +
		"&meta.long=" + strings.Repeat("x", 129) +
		"&other=ignored")
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"tenant": "0f8fad5b-d9cb-469f-a165-70867728950e",
		"camera": "vp/0f8fad5b-d9cb-469f-a165-70867728950e/Cam%2B1",
	}, ParseAuditMeta(query))

	require.Nil(t, ParseAuditMeta(url.Values{"src": {"x"}}))
}

func TestAuditIdentityKeepsFirstName(t *testing.T) {
	s := NewStream("rtsp://h/x")
	s.setAuditIdentity("rtsp://u:p@h/x?token=T", nil)
	s.setAuditIdentity("alias", nil)
	s.SetAuditMeta(map[string]string{"tenant": "a"})
	id := s.audit.Load()
	require.Equal(t, "rtsp://h/x", id.name)
	require.Equal(t, map[string]string{"tenant": "a"}, id.meta)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) events(t *testing.T) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if !strings.Contains(line, auditMessage) {
			continue
		}
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e), line)
		events = append(events, e)
	}
	return events
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev := log
	log = zerolog.New(buf).Level(zerolog.InfoLevel)
	t.Cleanup(func() { log = prev })
	return buf
}

type auditProducer struct {
	core.Connection
	startErr error
	done     chan struct{}
	once     sync.Once
}

func newAuditProducer(startErr error) *auditProducer {
	return &auditProducer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "audit",
			Medias: []*core.Media{{
				Kind:      core.KindVideo,
				Direction: core.DirectionRecvonly,
				Codecs:    []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}},
			}},
		},
		startErr: startErr,
		done:     make(chan struct{}),
	}
}

func (p *auditProducer) Start() error {
	if p.startErr != nil {
		return p.startErr
	}
	<-p.done
	return nil
}

func (p *auditProducer) Stop() error {
	p.once.Do(func() { close(p.done) })
	return p.Connection.Stop()
}

// pick returns the named fields of each event so assertions ignore timing values.
func pick(events []map[string]any, keys ...string) []map[string]any {
	out := make([]map[string]any, len(events))
	for i, e := range events {
		out[i] = map[string]any{}
		for _, k := range keys {
			if v, ok := e[k]; ok {
				out[i][k] = v
			}
		}
	}
	return out
}

const auditSource = "audit-test://admin:hunter2@cam.example:322/live/cam1?token=SECRET#video=copy"

func TestAuditProducerAndConsumerLifecycle(t *testing.T) {
	buf := captureLog(t)
	HandleFunc("audit-test", func(string) (core.Producer, error) { return newAuditProducer(nil), nil })
	t.Cleanup(func() { Delete("t-1a2b3c4d-session"); delete(handlers, "audit-test") })

	s, err := New("t-1a2b3c4d-session", auditSource)
	require.NoError(t, err)
	s.SetAuditMeta(map[string]string{"tenant": "1a2b3c4d", "camera": "cam1"})

	cons := mp4.NewConsumer(nil)
	cons.RemoteAddr = "127.0.0.1:50000"
	require.NoError(t, s.AddConsumer(cons))
	s.RemoveConsumer(cons)

	events := buf.events(t)
	require.Equal(t, []map[string]any{
		{"event": "producer_dial", "outcome": "connected", "attempt": 1.0, "trigger": "consumer"},
		{"event": "consumer_add", "outcome": "added", "type": "*mp4.Consumer", "format": "mp4", "remote": "127.0.0.1:50000"},
		{"event": "consumer_remove", "type": "*mp4.Consumer", "format": "mp4", "remote": "127.0.0.1:50000"},
		{"event": "producer_close", "reason": "no_consumers"},
	}, pick(events, "event", "outcome", "attempt", "trigger", "reason", "type", "format", "remote"))

	for _, e := range events {
		require.Equal(t, "t-1a2b3c4d-session", e["stream"])
		require.Equal(t, map[string]any{"camera": "cam1", "tenant": "1a2b3c4d"}, e["meta"])
		require.Equal(t, auditMessage, e["message"])
	}
	require.Equal(t, "audit-test://cam.example:322/live/cam1", events[0]["src"])
	require.Contains(t, events[3], "open_ms")
	require.Contains(t, events[3], "bytes_recv")

	out := buf.String()
	require.NotContains(t, out, "hunter2")
	require.NotContains(t, out, "SECRET")
}

func TestAuditReconnect(t *testing.T) {
	buf := captureLog(t)
	var dials atomic.Int32
	HandleFunc("audit-test", func(string) (core.Producer, error) {
		switch dials.Add(1) {
		case 1:
			return newAuditProducer(errors.New("read rtsps://admin:hunter2@cam.example/x?token=SECRET: EOF")), nil
		case 2:
			return nil, errors.New("dial rtsps://admin:hunter2@cam.example/x?token=SECRET: i/o timeout")
		default:
			return newAuditProducer(nil), nil
		}
	})
	t.Cleanup(func() { Delete("reconnect"); delete(handlers, "audit-test") })

	s, err := New("reconnect", auditSource)
	require.NoError(t, err)
	cons := mp4.NewConsumer(nil)
	require.NoError(t, s.AddConsumer(cons))

	require.Eventually(t, func() bool { return dials.Load() >= 3 && len(buf.events(t)) >= 5 }, 5*time.Second, 10*time.Millisecond)
	// reconnect logs the dial before moving tracks; wait for it to release the
	// producer before removing the consumer (stopProducers reads tracks unlocked).
	s.producers[0].mu.Lock()
	s.producers[0].mu.Unlock()
	s.RemoveConsumer(cons)

	events := buf.events(t)
	require.Equal(t, []map[string]any{
		{"event": "producer_dial", "outcome": "connected", "attempt": 1.0, "trigger": "consumer"},
		{"event": "consumer_add", "outcome": "added"},
		{"event": "producer_close", "reason": "error"},
		{"event": "producer_dial", "outcome": "error", "attempt": 2.0, "trigger": "reconnect", "retry": 0.0},
		{"event": "producer_dial", "outcome": "connected", "attempt": 3.0, "trigger": "reconnect", "retry": 1.0},
		{"event": "consumer_remove"},
		{"event": "producer_close", "reason": "no_consumers"},
	}, pick(events, "event", "outcome", "attempt", "trigger", "retry", "reason"))
	require.Equal(t, "read rtsps://cam.example/x: EOF", events[2]["error"])
	require.Equal(t, "dial rtsps://cam.example/x: i/o timeout", events[3]["error"])

	out := buf.String()
	require.NotContains(t, out, "hunter2")
	require.NotContains(t, out, "SECRET")
}

func TestAuditConsumerAddError(t *testing.T) {
	buf := captureLog(t)
	HandleFunc("audit-test", func(string) (core.Producer, error) {
		return nil, errors.New("dial rtsps://admin:hunter2@cam.example/x?token=SECRET: refused")
	})
	t.Cleanup(func() { Delete("fails"); delete(handlers, "audit-test") })

	s, err := New("fails", auditSource)
	require.NoError(t, err)
	require.Error(t, s.AddConsumer(mp4.NewConsumer(nil)))

	require.Equal(t, []map[string]any{
		{"event": "producer_dial", "outcome": "error", "attempt": 1.0},
		{"event": "consumer_add", "outcome": "error"},
	}, pick(buf.events(t), "event", "outcome", "attempt"))
	require.NotContains(t, buf.String(), "hunter2")
	require.NotContains(t, buf.String(), "SECRET")
}
