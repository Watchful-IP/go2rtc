package streams

import (
	"net/url"
	"strings"
)

// Keep this shared by logging and disconnected-producer JSON. Exacq recorder
// and SOCKS credentials, and SDK capability paths, must not leave the source.
func redactProducerURL(raw string) string {
	scheme, _, _ := strings.Cut(raw, ":")
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "exacq" && scheme != "exacqsdk" && scheme != "exacqsdks" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return scheme + "://[redacted]"
	}
	if scheme != "exacq" {
		return scheme + "://" + u.Host + "/internal/stream/[redacted]"
	}
	if u.User != nil {
		u.User = url.UserPassword("xxx", "xxx")
	}
	u.Fragment, u.RawFragment = "", ""
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		u.RawQuery = "[redacted]"
		return u.String()
	}
	for key := range q {
		switch key {
		case "stream", "start", "end", "speed", "gaps":
		default:
			q.Set(key, "[redacted]")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
