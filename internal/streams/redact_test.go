package streams

import (
	"strings"
	"testing"
)

func TestRedactExacqProducerURL(t *testing.T) {
	for _, raw := range []string{
		"exacq://admin:recorderSecret@recorder/459264?proxy=socks5%3A%2F%2Froute%3AproxySecret%40gateway%3A1080&start=1700000000000",
		"exacq://admin:recorderSecret@recorder/459264?proxy=bad%zz",
		"exacq://admin:recorderSecret@recorder:bad/459264",
		"exacqsdk://gateway/internal/stream/bearerSecret?speed=1",
		"exacq://admin:recorderSecret@host/1?PROXY=proxySecret&unknown=bearerSecret#recorderSecret",
		"exacqsdks://gateway/internal/stream/bearerSecret",
		"EXACQ://admin:recorderSecret@recorder/459264",
		"eXaCqSdKs://gateway/internal/stream/bearerSecret",
		" exacq://admin:recorderSecret@recorder/459264",
		"exacq:admin:recorderSecret@recorder/459264",
	} {
		got := redactProducerURL(raw)
		for _, secret := range []string{"recorderSecret", "proxySecret", "bearerSecret"} {
			if strings.Contains(got, secret) {
				t.Fatal("source credential leaked")
			}
		}
	}
	if got := redactProducerURL("rtsp://example/video"); got != "rtsp://example/video" {
		t.Fatal(got)
	}
}
