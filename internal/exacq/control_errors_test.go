package exacq

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	native "github.com/AlexxIT/go2rtc/pkg/exacq"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func post(t *testing.T, s controlServer, body string, header map[string]string) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.serve(w, r)
	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &decoded), w.Body.String())
	return w, decoded
}

func TestControlErrorEnvelopeAndStatus(t *testing.T) {
	status := -3
	for _, tc := range []struct {
		err    *native.Error
		status int
		want   string
	}{
		{&native.Error{Code: native.CodeAuthInvalidCredentials, Phase: "login", Message: "recorder rejected the username or password", RecorderStatus: &status},
			422, `{"code":"auth_invalid_credentials","message":"recorder rejected the username or password","phase":"login","retryable":false,"recorderStatus":-3}`},
		{&native.Error{Code: native.CodeUnreachable, Phase: "dial", Message: "connection refused by recorder:22609", Retryable: true},
			502, `{"code":"unreachable","message":"connection refused by recorder:22609","phase":"dial","retryable":true}`},
		{&native.Error{Code: native.CodeTimeout, Phase: "dial", Message: "timed out", Retryable: true}, 504, ""},
		{&native.Error{Code: native.CodeCameraNotFound, Phase: "live", Message: "camera 1 is not on this recorder"}, 404, ""},
		{&native.Error{Code: native.CodeDecoderUnavailable, Phase: "snapshot", Message: "no ffmpeg"}, 503, ""},
		{&native.Error{Code: native.CodeConfigNotPersisted, Phase: "webhook", Message: "retry", Retryable: true}, 504, ""},
		{&native.Error{Code: native.CodeNotExacq, Phase: "handshake", Message: "HTTP"}, 502, ""},
	} {
		s := controlServer{connect: func(context.Context, native.Options) (controlClient, error) { return nil, tc.err }}
		w, body := post(t, s, `{"action":"inventory","address":"recorder:22609","username":"admin","password":"secret"}`, nil)
		require.Equal(t, tc.status, w.Code, tc.err.Code)
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))
		if tc.want != "" {
			require.JSONEq(t, tc.want, string(body["error"]))
		}
	}
}

func TestControlValidationIsCodedAndSpecific(t *testing.T) {
	s := controlServer{connect: func(context.Context, native.Options) (controlClient, error) {
		t.Fatal("invalid request dialed recorder")
		return nil, nil
	}}
	w, body := post(t, s, `{"action":"teleport","address":"recorder:22609","username":"admin","password":"secret"}`, nil)
	require.Equal(t, 400, w.Code)
	require.JSONEq(t, `{"code":"invalid_request","message":"unknown action \"teleport\"","phase":"request","retryable":false}`, string(body["error"]))
	w, body = post(t, s, `{"action":"health","address":"recorder:22609","username":"admin","password":"secret","bogus":1}`, nil)
	require.Equal(t, 400, w.Code)
	require.Contains(t, string(body["error"]), `unknown field \"bogus\"`)
	require.NotContains(t, w.Body.String(), "secret")
}

func TestControlRequestIDAndServerTiming(t *testing.T) {
	s := controlServer{connect: func(_ context.Context, options native.Options) (controlClient, error) {
		options.Trace.Observe("login", time.Now().Add(-40*time.Millisecond))
		return &fakeControl{}, nil
	}}
	w, _ := post(t, s, `{"action":"inventory","address":"recorder:22609","username":"admin","password":"secret"}`, map[string]string{"X-Request-Id": "req-123"})
	require.Equal(t, 200, w.Code)
	require.Equal(t, "req-123", w.Header().Get("X-Request-Id"))
	timing := w.Header().Get("Server-Timing")
	require.Regexp(t, `^login;dur=4\d\.\d, action;dur=[\d.]+, total;dur=[\d.]+$`, timing)

	w, _ = post(t, s, `{"action":"inventory","address":"recorder:22609","username":"admin","password":"secret"}`, map[string]string{"X-Request-Id": "bad id\n"})
	require.Regexp(t, `^[0-9a-f]{16}$`, w.Header().Get("X-Request-Id"))
}

func TestControlDiagnoseReportsFailingStep(t *testing.T) {
	status := -3
	s := controlServer{diagnose: func(_ context.Context, options native.Options) *native.Diagnosis {
		require.Equal(t, "recorder:22609", options.Address)
		return &native.Diagnosis{Recorder: &native.RecorderIdentity{Serial: "00-15"}, Steps: []native.DiagnosisStep{
			{Step: "dial", OK: true, MS: 3, Detail: "connected directly to recorder:22609"},
			{Step: "handshake", OK: true, MS: 2, Detail: "exacqVision Server 00-15 answered; login uses SPAKE2+"},
			{Step: "login", MS: 40, Err: &native.Error{Code: native.CodeAuthInvalidCredentials, Phase: "login", Message: "recorder rejected the username or password", RecorderStatus: &status}},
		}}
	}}
	w, body := post(t, s, `{"action":"diagnose","address":"recorder:22609","username":"admin","password":"secret"}`, nil)
	require.Equal(t, 200, w.Code)
	require.JSONEq(t, `false`, string(body["ok"]))
	require.JSONEq(t, `{"serial":"00-15"}`, string(body["recorder"]))
	var steps []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body["steps"], &steps))
	require.Len(t, steps, 3)
	require.NotContains(t, steps[0], "error")
	require.JSONEq(t, `{"code":"auth_invalid_credentials","message":"recorder rejected the username or password","phase":"login","retryable":false,"recorderStatus":-3}`, string(steps[2]["error"]))
}

func TestControlRedactionKeepsUsernameInClassifiedMessages(t *testing.T) {
	s := controlServer{connect: func(context.Context, native.Options) (controlClient, error) {
		return nil, &native.Error{Code: native.CodeConfigRejected, Phase: "webhook", Message: "the account may need Setup or administrator permission"}
	}}
	_, body := post(t, s, `{"action":"inventory","address":"recorder:22609","username":"admin","password":"secret"}`, nil)
	require.Contains(t, string(body["error"]), "administrator permission")
}

func TestControlFailuresLogAtWarnWithMessage(t *testing.T) {
	var buf strings.Builder
	saved := log
	log = zerolog.New(&buf).Level(zerolog.InfoLevel)
	defer func() { log = saved }()
	cause := &native.Error{Code: native.CodeNoRecording, Phase: "snapshot", Message: "no decodable picture for secret-pass"}
	s := controlServer{connect: func(context.Context, native.Options) (controlClient, error) { return nil, cause }}
	w, _ := post(t, s, `{"action":"inventory","address":"recorder:22609","username":"admin","password":"secret-pass"}`, nil)
	require.Equal(t, 422, w.Code)
	line := buf.String()
	require.Contains(t, line, `"level":"warn"`)
	require.Contains(t, line, `"outcome":"no_recording"`)
	require.Contains(t, line, `"error":"no decodable picture for [redacted]"`)
	require.NotContains(t, line, "secret-pass")
}
