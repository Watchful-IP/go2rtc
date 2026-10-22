package exacq

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	native "github.com/AlexxIT/go2rtc/pkg/exacq"
)

// Recorder credentials are carried only in the POST body. Access control and
// private binding are inherited from the existing go2rtc API configuration.
type controlRequest struct {
	Action    string          `json:"action"`
	Address   string          `json:"address"`
	Username  string          `json:"username"`
	Password  string          `json:"password"`
	Proxy     string          `json:"proxy,omitempty"`
	CameraID  uint32          `json:"cameraId,omitempty"`
	CameraIDs []uint32        `json:"cameraIds,omitempty"`
	OwnerID   string          `json:"ownerId,omitempty"`
	StartMS   *int64          `json:"startMs,omitempty"`
	EndMS     *int64          `json:"endMs,omitempty"`
	AtMS      *int64          `json:"atMs,omitempty"`
	Width     int             `json:"width,omitempty"`
	Frequency int             `json:"frequency,omitempty"`
	PCMBase64 string          `json:"pcmBase64,omitempty"`
	Webhook   *webhookRequest `json:"webhook,omitempty"`
}

type controlClient interface {
	Close() error
	Inventory() *native.Inventory
	RefreshStatus(context.Context) (*native.Status, error)
	ReadLicense(context.Context) (*native.License, error)
	Search(context.Context, uint32, time.Time, time.Time) ([]*native.MediaPacket, error)
	SendAudio(context.Context, uint32, []byte, int) error
	Snapshot(context.Context, uint32, int, *time.Time) (*native.Snapshot, error)
}

type connector func(context.Context, native.Options) (controlClient, error)

// controlServer holds the recorder-facing operations so tests can replace them.
// Webhook apply and diagnose manage their own sessions.
type controlServer struct {
	connect      connector
	applyWebhook func(context.Context, native.Options, native.WebhookConfig) (*native.WebhookPlan, error)
	diagnose     func(context.Context, native.Options) *native.Diagnosis
}

var controlActions = map[string]bool{
	"inventory": true, "health": true, "diagnose": true, "webhook.plan": true, "webhook.apply": true,
	"talkdown": true, "playback.prepare": true, "snapshot": true,
}

func controlHandler(w http.ResponseWriter, r *http.Request) {
	serveControl(w, r, func(ctx context.Context, options native.Options) (controlClient, error) {
		return native.Connect(ctx, options)
	})
}

func serveControl(w http.ResponseWriter, r *http.Request, connect connector) {
	controlServer{connect: connect, applyWebhook: native.ApplyWebhook, diagnose: native.Diagnose}.serve(w, r)
}

// controlCall is one request's diagnostic context.
type controlCall struct {
	id      string
	started time.Time
	request controlRequest
	trace   *native.Trace
}

func (s controlServer) serve(w http.ResponseWriter, r *http.Request) {
	call := &controlCall{id: requestID(r.Header.Get("X-Request-Id")), started: time.Now(), trace: &native.Trace{}}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-Id", call.id)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		call.fail(w, http.StatusMethodNotAllowed, invalid("POST required"))
		return
	}
	if r.URL.RawQuery != "" {
		call.fail(w, 0, invalid("Exacq arguments belong in the JSON body, not the query string"))
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		call.fail(w, http.StatusUnsupportedMediaType, invalid("JSON body required"))
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&call.request); err != nil {
		// Decoder errors name fields, never values.
		call.request = controlRequest{}
		call.fail(w, 0, invalid("invalid JSON body: "+strings.TrimPrefix(err.Error(), "json: ")))
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		call.fail(w, 0, invalid("JSON body must contain exactly one object"))
		return
	}
	if err := call.request.validate(); err != nil {
		call.fail(w, 0, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	result, err := s.run(ctx, call)
	if err != nil {
		call.fail(w, 0, err)
		return
	}
	call.respond(w, http.StatusOK, result, "ok")
}

func (request controlRequest) validate() error {
	if request.Address == "" || request.Username == "" || request.Password == "" {
		return invalid("recorder address, username and password are required")
	}
	if !controlActions[request.Action] {
		return invalid(fmt.Sprintf("unknown action %q", request.Action))
	}
	if request.Webhook != nil && request.Action != "webhook.plan" && request.Action != "webhook.apply" {
		return invalid("webhook configuration requires a webhook action")
	}
	return nil
}

func (s controlServer) run(ctx context.Context, call *controlCall) (any, error) {
	request := call.request
	options := native.Options{Address: request.Address, Username: request.Username, Password: request.Password, ProxyURL: request.Proxy, Timeout: 15 * time.Second, Trace: call.trace}
	started := time.Now()
	defer func() { call.trace.Observe("action", started) }()
	switch request.Action {
	case "diagnose":
		return s.diagnose(ctx, options), nil
	case "webhook.apply":
		plan, err := s.applyWebhook(ctx, options, request.webhookConfig())
		if err != nil {
			return nil, err
		}
		if !plan.Configured {
			return nil, &native.Error{Code: native.CodeConfigNotPersisted, Phase: native.PhaseWebhook, Message: "webhook configuration was not verified", Retryable: true}
		}
		return struct {
			Configured bool                 `json:"configured"`
			Verified   bool                 `json:"verified"`
			Create     int                  `json:"create"`
			Update     int                  `json:"update"`
			Delete     int                  `json:"delete"`
			Links      []native.WebhookLink `json:"links"`
			Changes    []string             `json:"changes"`
		}{true, true, plan.Create, plan.Update, plan.Delete, plan.Links, plan.Changes}, nil
	}
	c, err := s.connect(ctx, options)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	started = time.Now() // The action phase excludes session setup.
	switch request.Action {
	case "inventory":
		_, err = c.RefreshStatus(ctx)
		return c.Inventory(), err
	case "health":
		status, err := c.RefreshStatus(ctx)
		if err != nil {
			return nil, err
		}
		license, err := c.ReadLicense(ctx)
		return struct {
			Status    *native.Status    `json:"status"`
			Inventory *native.Inventory `json:"inventory"`
			License   *native.License   `json:"license"`
		}{status, c.Inventory(), license}, err
	case "webhook.plan":
		planner, ok := c.(webhookPlanner)
		if !ok {
			return nil, errors.New("native webhook planning unavailable")
		}
		return planner.PlanWebhook(request.webhookConfig())
	case "snapshot":
		var at *time.Time
		if request.AtMS != nil {
			t := time.UnixMilli(*request.AtMS)
			at = &t
		}
		snapshot, err := c.Snapshot(ctx, request.CameraID, request.Width, at)
		if err != nil {
			return nil, err
		}
		return map[string]any{"jpegBase64": base64.StdEncoding.EncodeToString(snapshot.JPEG), "capturedAtMs": snapshot.CapturedAt.UnixMilli(), "width": snapshot.Width, "height": snapshot.Height}, nil
	case "talkdown":
		pcm, err := base64.StdEncoding.DecodeString(request.PCMBase64)
		if err != nil {
			return nil, invalid("pcmBase64 is not valid base64")
		}
		if err = c.SendAudio(ctx, request.CameraID, pcm, request.Frequency); err != nil {
			return nil, err
		}
		return map[string]any{"accepted": true, "acceptedBytes": len(pcm)}, nil
	case "playback.prepare":
		start, end, err := request.interval()
		if err != nil {
			return nil, err
		}
		return preparePlayback(ctx, c, request.CameraID, start, end)
	}
	return nil, invalid("unknown action")
}

func preparePlayback(ctx context.Context, c controlClient, camera uint32, start, end time.Time) (map[string]int64, error) {
	for cursor := start; cursor.Before(end); {
		boundary := cursor.Add(10 * time.Second)
		if boundary.After(end) {
			boundary = end
		}
		frames, err := c.Search(ctx, camera, cursor, boundary)
		if err != nil {
			return nil, err
		}
		if stamp, ok := native.PlaybackStart(frames, start); ok && stamp.Before(end) {
			return map[string]int64{"startsAtMs": stamp.UnixMilli()}, nil
		}
		cursor = boundary
	}
	return nil, &native.Error{Code: native.CodeNoRecording, Phase: native.PhaseSearch, Message: fmt.Sprintf("camera %d has no recorded keyframe between %s and %s", camera, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))}
}

func (request controlRequest) interval() (time.Time, time.Time, error) {
	if request.StartMS == nil || request.EndMS == nil {
		return time.Time{}, time.Time{}, invalid("startMs and endMs are required")
	}
	start, end := time.UnixMilli(*request.StartMS), time.UnixMilli(*request.EndMS)
	if start.Unix() < 0 || end.Unix() > 2147483647 || !end.After(start) || end.Sub(start) > time.Hour {
		return time.Time{}, time.Time{}, invalid("recording interval must be positive, at most one hour, and before 2038")
	}
	return start, end, nil
}

func invalid(message string) error {
	return &native.Error{Code: native.CodeInvalidRequest, Phase: native.PhaseRequest, Message: message}
}

func requestID(header string) string {
	if len(header) > 0 && len(header) <= 128 {
		valid := true
		for _, b := range []byte(header) {
			valid = valid && b > 32 && b < 127
		}
		if valid {
			return header
		}
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
