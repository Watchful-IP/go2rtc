package streams

// Watchful: stream-audit logs one INFO line per upstream dial, upstream close and
// consumer add/remove, so every upstream session go2rtc opens can be reconciled
// row by row against a vendor's logs. Source URLs are always redacted to
// scheme://host/path: userinfo, query strings and #options never reach the log.

import (
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/rs/zerolog"
)

const auditMessage = "stream-audit"

// auditIdentity is immutable; Stream swaps the whole value on change.
type auditIdentity struct {
	name string
	meta map[string]string
}

var auditMu sync.Mutex

// setAuditIdentity records the stream name the first time it is set (aliases keep
// the original name) and replaces meta when a non-empty map is given.
func (s *Stream) setAuditIdentity(name string, meta map[string]string) {
	auditMu.Lock()
	defer auditMu.Unlock()

	id := auditIdentity{}
	if cur := s.audit.Load(); cur != nil {
		id = *cur
	}
	if id.name == "" {
		id.name = RedactURL(name)
	}
	if len(meta) > 0 {
		id.meta = meta
	}
	s.audit.Store(&id)
}

// SetAuditMeta tags the stream with caller identity for stream-audit lines.
func (s *Stream) SetAuditMeta(meta map[string]string) {
	if len(meta) > 0 {
		s.setAuditIdentity("", meta)
	}
}

func (s *Stream) auditEvent(event string) *zerolog.Event {
	e := log.Info().Str("event", event)
	if s == nil {
		return e
	}
	if id := s.audit.Load(); id != nil {
		e = e.Str("stream", id.name)
		if len(id.meta) > 0 {
			dict := zerolog.Dict()
			for _, k := range slices.Sorted(maps.Keys(id.meta)) {
				dict = dict.Str(k, id.meta[k])
			}
			e = e.Dict("meta", dict)
		}
	}
	return e
}

const (
	auditMetaPrefix   = "meta."
	auditMetaMaxKeys  = 8
	auditMetaMaxValue = 128
)

var (
	auditMetaKey   = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
	auditMetaValue = regexp.MustCompile(`^[A-Za-z0-9._:/@=+%-]+$`)
)

// ParseAuditMeta collects meta.<key>=<value> query params. Keys and values are
// restricted to a safe charset so callers cannot inject into log lines; invalid
// pairs are dropped rather than rewritten.
func ParseAuditMeta(query url.Values) map[string]string {
	var meta map[string]string
	for k, values := range query {
		key, ok := strings.CutPrefix(k, auditMetaPrefix)
		if !ok || len(values) == 0 || !auditMetaKey.MatchString(key) {
			continue
		}
		value := values[0]
		if len(value) > auditMetaMaxValue || !auditMetaValue.MatchString(value) {
			continue
		}
		if meta == nil {
			meta = map[string]string{}
		}
		if len(meta) == auditMetaMaxKeys {
			break
		}
		meta[key] = value
	}
	return meta
}

// Userinfo is matched greedily up to the last '@' before the path, so passwords
// containing '@' are still removed whole. Quotes end a URL (Go quotes URLs in
// errors), and a query never ends with ':' so "url?q=1: EOF" keeps its separator.
var auditURL = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)(?:[^/\s"']*@)?([^\s?#"']*)(?:\?[^\s#"']*[^\s#"':])?(?:#[^\s"']*)?`)

// RedactURL reduces every URL in s to scheme://host[:port]/path, dropping userinfo,
// query and fragment. Works on go2rtc sources (ffmpeg:rtsp://u:p@h/x#video=copy)
// and on free text such as error messages.
func RedactURL(s string) string {
	return auditURL.ReplaceAllString(s, "$1$2")
}

func redactErr(err error) string {
	return RedactURL(err.Error())
}

// producer events, all called with p.mu held

func (p *Producer) auditEvent(event string) *zerolog.Event {
	return p.stream.auditEvent(event).Str("src", RedactURL(p.url))
}

func (p *Producer) auditDial(trigger string, retry int) (core.Producer, error) {
	p.dials++
	start := time.Now()
	conn, err := GetProducer(p.url)

	e := p.auditEvent("producer_dial").
		Int("attempt", p.dials).
		Str("trigger", trigger).
		Int64("duration_ms", time.Since(start).Milliseconds())
	if trigger == "reconnect" {
		e = e.Int("retry", retry)
	}
	if err != nil {
		e.Str("outcome", "error").Str("error", redactErr(err)).Msg(auditMessage)
		return nil, err
	}
	e.Str("outcome", "connected").Msg(auditMessage)

	p.openedAt = time.Now()
	return conn, nil
}

func (p *Producer) auditClose(reason string, err error) {
	var recv int
	for _, track := range p.receivers {
		recv += track.Bytes
	}

	e := p.auditEvent("producer_close").Str("reason", reason).Int("bytes_recv", recv)
	if !p.openedAt.IsZero() {
		e = e.Int64("open_ms", time.Since(p.openedAt).Milliseconds())
	}
	if err != nil {
		e = e.Str("error", redactErr(err))
	}
	e.Msg(auditMessage)
}

// consumer events

func (s *Stream) auditConsumer(event string, cons core.Consumer) *zerolog.Event {
	e := s.auditEvent(event).Str("type", reflect.TypeOf(cons).String())
	if conn := consumerConnection(cons); conn != nil {
		e = e.Uint32("consumer_id", conn.ID).
			Str("format", conn.FormatName).
			Str("protocol", conn.Protocol).
			Str("remote", conn.RemoteAddr).
			Str("user_agent", conn.UserAgent)
	}
	return e
}

var connectionType = reflect.TypeOf(core.Connection{})

// consumerConnection finds the embedded core.Connection that nearly every consumer
// carries, without adding an accessor to every consumer type.
func consumerConnection(cons core.Consumer) *core.Connection {
	v := reflect.ValueOf(cons)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil
	}
	f := v.FieldByName("Connection")
	if !f.IsValid() || f.Type() != connectionType || !f.CanAddr() {
		return nil
	}
	return f.Addr().Interface().(*core.Connection)
}
