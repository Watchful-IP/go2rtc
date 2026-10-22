package exacq

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	native "github.com/AlexxIT/go2rtc/pkg/exacq"
	"github.com/rs/zerolog"
)

var log = zerolog.Nop()

// errorObject is the public failure contract shared with the ihub adapter.
type errorObject struct {
	Code           native.Code              `json:"code"`
	Message        string                   `json:"message"`
	Phase          string                   `json:"phase,omitempty"`
	Retryable      bool                     `json:"retryable"`
	RecorderStatus *int                     `json:"recorderStatus,omitempty"`
	Recorder       *native.RecorderIdentity `json:"recorder,omitempty"`
}

type diagnosisStep struct {
	native.DiagnosisStep
	Error *errorObject `json:"error,omitempty"`
}

var codeStatus = map[native.Code]int{
	native.CodeInvalidRequest:     http.StatusBadRequest,
	native.CodeCameraNotFound:     http.StatusNotFound,
	native.CodeConfigConflict:     http.StatusConflict,
	native.CodeBusy:               http.StatusConflict,
	native.CodePermissionDenied:   http.StatusUnprocessableEntity,
	native.CodeStreamUnavailable:  http.StatusUnprocessableEntity,
	native.CodeUnsupportedCodec:   http.StatusUnprocessableEntity,
	native.CodeNoRecording:        http.StatusUnprocessableEntity,
	native.CodeWebhookUnsupported: http.StatusUnprocessableEntity,
	native.CodeConfigRejected:     http.StatusUnprocessableEntity,
	native.CodeUnreachable:        http.StatusBadGateway,
	native.CodeProxyFailed:        http.StatusBadGateway,
	native.CodeNotExacq:           http.StatusBadGateway,
	native.CodeProtocolError:      http.StatusBadGateway,
	native.CodeConnectionLost:     http.StatusBadGateway,
	native.CodeDecoderUnavailable: http.StatusServiceUnavailable,
	native.CodeTimeout:            http.StatusGatewayTimeout,
	native.CodeConfigNotPersisted: http.StatusGatewayTimeout,
	native.CodeInternal:           http.StatusInternalServerError,
}

func httpStatus(code native.Code) int {
	if status, found := codeStatus[code]; found {
		return status
	}
	if strings.HasPrefix(string(code), "auth_") {
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// object classifies err for the wire. Unclassified errors are internal; every
// message passes through redaction because some causes quote their input.
func (call *controlCall) object(err error) *errorObject {
	o := &errorObject{Code: native.CodeInternal, Phase: native.PhaseRequest, Message: strings.TrimPrefix(err.Error(), "exacq: ")}
	if e := native.AsError(err); e != nil {
		o.Code, o.Phase, o.Message, o.Retryable, o.RecorderStatus = e.Code, e.Phase, e.Message, e.Retryable, e.RecorderStatus
	}
	// Classified messages are composed without request input, so the username
	// (often "admin") is only scrubbed from messages that quote a cause.
	o.Message = call.redact(o.Message, o.Code == native.CodeInternal || o.Code == native.CodeProtocolError)
	if serial, version := call.trace.Recorder(); serial != "" || version != "" {
		o.Recorder = &native.RecorderIdentity{Serial: serial, Version: version}
	}
	return o
}

// fail writes the error envelope; status 0 derives it from the code.
func (call *controlCall) fail(w http.ResponseWriter, status int, err error) {
	o := call.object(err)
	if status == 0 {
		status = httpStatus(o.Code)
	}
	call.respond(w, status, map[string]*errorObject{"error": o}, string(o.Code))
}

func (call *controlCall) respond(w http.ResponseWriter, status int, body any, outcome string) {
	if d, ok := body.(*native.Diagnosis); ok {
		body = call.diagnosis(d)
		if !d.OK {
			outcome = "diagnose_failed"
		}
	}
	total := time.Since(call.started)
	timing := call.trace.Phases()
	w.Header().Set("Server-Timing", serverTiming(timing, total))
	w.Header().Set("Content-Type", api.MimeJSON)
	w.WriteHeader(status)
	api.ResponseJSON(w, body)
	message := ""
	if e, ok := body.(map[string]*errorObject); ok && e["error"] != nil {
		message = e["error"].Message
	}
	call.log(status, outcome, message, timing, total)
}

func (call *controlCall) diagnosis(d *native.Diagnosis) any {
	steps := make([]diagnosisStep, len(d.Steps))
	for i, step := range d.Steps {
		step.Detail = call.redact(step.Detail, false)
		steps[i] = diagnosisStep{DiagnosisStep: step}
		if step.Err != nil {
			steps[i].Error = call.object(step.Err)
		}
	}
	return struct {
		OK       bool                     `json:"ok"`
		Recorder *native.RecorderIdentity `json:"recorder,omitempty"`
		Steps    []diagnosisStep          `json:"steps"`
	}{d.OK, d.Recorder, steps}
}

func serverTiming(phases []native.PhaseTiming, total time.Duration) string {
	parts := make([]string, 0, len(phases)+1)
	for _, p := range phases {
		parts = append(parts, fmt.Sprintf("%s;dur=%.1f", p.Phase, float64(p.Duration.Microseconds())/1000))
	}
	parts = append(parts, fmt.Sprintf("total;dur=%.1f", float64(total.Microseconds())/1000))
	return strings.Join(parts, ", ")
}

// Failures log at warn with their (already redacted) message: recorder-side 4xx
// such as rejected credentials are exactly what an operator needs to see.
func (call *controlCall) log(status int, outcome, message string, phases []native.PhaseTiming, total time.Duration) {
	event := log.Info()
	if status >= 400 || outcome == "diagnose_failed" {
		event = log.Warn()
	}
	if message != "" {
		event = event.Str("error", message)
	}
	serial, version := call.trace.Recorder()
	durations := zerolog.Dict()
	for _, p := range phases {
		durations.Int64(p.Phase, p.Duration.Milliseconds())
	}
	event.Str("request_id", call.id).Str("action", call.request.Action).Str("recorder", call.request.Address).
		Bool("proxied", call.request.Proxy != "").Str("serial", serial).Str("version", version).
		Int("status", status).Str("outcome", outcome).Int("connects", call.trace.Connects()).
		Int64("ms", total.Milliseconds()).Dict("phase_ms", durations).Msg("[exacq] control")
}

// redact removes request secrets, in raw and escaped forms, from text that
// may quote a cause.
func (call *controlCall) redact(message string, username bool) string {
	request := call.request
	secrets := []string{request.Password, request.Proxy, request.PCMBase64}
	if username {
		secrets = append(secrets, request.Username)
	}
	if u, err := url.Parse(request.Proxy); err == nil && u.User != nil {
		password, _ := u.User.Password()
		secrets = append(secrets, u.User.Username(), password)
	}
	if request.Webhook != nil {
		secrets = append(secrets, request.Webhook.URL, request.Webhook.Authorization)
		if _, token, found := strings.Cut(request.Webhook.Authorization, " "); found {
			secrets = append(secrets, token)
		}
		if u, err := url.Parse(request.Webhook.URL); err == nil {
			secrets = append(secrets, u.RequestURI())
		}
	}
	for _, secret := range secrets {
		if secret == "" || secret == "/" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		message = strings.ReplaceAll(message, url.QueryEscape(secret), "[redacted]")
		message = strings.ReplaceAll(message, url.PathEscape(secret), "[redacted]")
	}
	return message
}
