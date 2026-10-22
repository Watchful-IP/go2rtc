package exacq

import (
	"context"
	"encoding/json"
	"errors"
	native "github.com/AlexxIT/go2rtc/pkg/exacq"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeControl struct {
	closed       bool
	snapshotTime *time.Time
	refreshed    bool
	searches     []time.Time
	frames       []*native.MediaPacket
}

func (f *fakeControl) Close() error                 { f.closed = true; return nil }
func (f *fakeControl) Inventory() *native.Inventory { return &native.Inventory{} }
func (f *fakeControl) RefreshStatus(context.Context) (*native.Status, error) {
	f.refreshed = true
	return &native.Status{}, nil
}
func (f *fakeControl) ReadLicense(context.Context) (*native.License, error) {
	return &native.License{Status: "trial"}, nil
}
func (f *fakeControl) Search(_ context.Context, _ uint32, start, _ time.Time) ([]*native.MediaPacket, error) {
	f.searches = append(f.searches, start)
	if len(f.searches) < 3 {
		return nil, nil
	}
	return f.frames, nil
}

func TestPreparePlaybackScansRecordingGap(t *testing.T) {
	start := time.UnixMilli(1700000000000)
	f := &fakeControl{frames: []*native.MediaPacket{{Type: native.MediaIFrame, Seconds: 1700000021, Milliseconds: 432}}}
	result, err := preparePlayback(context.Background(), f, 459264, start, start.Add(50*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result["startsAtMs"] != 1700000021432 || len(f.searches) != 3 || !f.searches[2].Equal(start.Add(20*time.Second)) {
		t.Fatal("gap scanning lost first available keyframe")
	}
}
func (f *fakeControl) SendAudio(context.Context, uint32, []byte, int) error { return nil }
func (f *fakeControl) Snapshot(_ context.Context, _ uint32, _ int, at *time.Time) (*native.Snapshot, error) {
	f.snapshotTime = at
	return &native.Snapshot{JPEG: []byte{1, 2, 3}, CapturedAt: *at, Width: 1280, Height: 720}, nil
}

func TestControlRequiresBodyOnlyPOST(t *testing.T) {
	for _, tc := range []struct {
		method, query, mime, body string
		status                    int
	}{
		{"GET", "", "", "", 405},
		{"POST", "?password=secret", "application/json", `{}`, 400},
		{"POST", "", "text/plain", `{}`, 415},
		{"POST", "", "application/json", `{"password":"secret","unknown":1}`, 400},
		{"POST", "", "application/json", `{} {}`, 400},
		{"POST", "", "application/json", `{"action":"bad","address":"host:1","username":"name","password":"secret"}`, 400},
		{"POST", "", "application/json", `{"action":"snapshot","address":"host:1","username":"name","password":"secret","reorder":"guess"}`, 400},
		{"POST", "", "application/json", `{"action":"health","address":"host:1","username":"name","password":"secret","reorder":"gop"}`, 400},
		{"POST", "", "application/json", `{"action":"snapshot","address":"host:1","username":"name","password":"secret","reorder":"gop"}`, 400},
		{"POST", "", "application/json", `{"action":"snapshot","address":"host:1","username":"name","password":"secret","gopFrames":15}`, 400},
	} {
		r := httptest.NewRequest(tc.method, "/api/exacq"+tc.query, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.mime)
		w := httptest.NewRecorder()
		serveControl(w, r, func(context.Context, native.Options) (controlClient, error) {
			t.Fatal("invalid request dialed recorder")
			return nil, nil
		})
		if w.Code != tc.status {
			t.Fatalf("got%d want%d", w.Code, tc.status)
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("echoed credentials")
		}
	}
}

func TestControlRoutingSnapshotAndCleanup(t *testing.T) {
	at := int64(1700000000123)
	body, _ := json.Marshal(controlRequest{Action: "snapshot", Address: "recorder:22609", Username: "name", Password: "secret", Proxy: "socks5://route:bridge@gateway:1080", CameraID: 459264, AtMS: &at, Width: 1280})
	f := &fakeControl{}
	r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveControl(w, r, func(ctx context.Context, options native.Options) (controlClient, error) {
		if options.ProxyURL != "socks5://route:bridge@gateway:1080" || options.Password != "secret" {
			t.Fatal("lost recorder routing or credentials")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing request deadline")
		}
		return f, nil
	})
	if w.Code != http.StatusOK || !f.closed || f.snapshotTime == nil || f.snapshotTime.UnixMilli() != at {
		t.Fatalf("snapshot forwarding or cleanup failed: %d", w.Code)
	}
	var result struct {
		JPEG string `json:"jpegBase64"`
		At   int64  `json:"capturedAtMs"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.JPEG != "AQID" || result.At != at {
		t.Fatal("invalid snapshot response")
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("response echoed credentials")
	}
}

func TestControlRedactsTransportError(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(`{"action":"inventory","address":"recorder:22609","username":"name","password":"secret!"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveControl(w, r, func(context.Context, native.Options) (controlClient, error) {
		return nil, errors.New("failed secret! secret%21 name")
	})
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "name") || !strings.Contains(w.Body.String(), `"code":"internal"`) {
		t.Fatalf("unsafe connection error: %d %s", w.Code, w.Body.String())
	}
}
