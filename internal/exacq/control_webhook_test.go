package exacq

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	native "github.com/AlexxIT/go2rtc/pkg/exacq"
	"github.com/stretchr/testify/require"
)

type fakeWebhookControl struct {
	fakeControl
	config native.WebhookConfig
}

func (f *fakeWebhookControl) PlanWebhook(config native.WebhookConfig) (*native.WebhookPlan, error) {
	f.config = config
	return &native.WebhookPlan{Links: []native.WebhookLink{{EventID: 42, CameraID: 459264}}, Changes: []string{"rotate callback authorization"}, Update: 1}, nil
}

const webhookBody = `{"action":"%s","address":"recorder:22609","username":"admin","password":"secret","ownerId":"owner","cameraIds":[459264],"webhook":{"url":"https://receiver.example/hook?token=private","authorization":"Bearer private"}}`

func TestWebhookPlanUsesOneSessionAndExplainsChanges(t *testing.T) {
	f := &fakeWebhookControl{}
	r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(strings.Replace(webhookBody, "%s", "webhook.plan", 1)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	connects := 0
	controlServer{connect: func(context.Context, native.Options) (controlClient, error) { connects++; return f, nil }}.serve(w, r)
	require.Equal(t, 200, w.Code)
	require.Equal(t, 1, connects)
	require.True(t, f.closed)
	require.Equal(t, "Bearer private", f.config.Authorization)
	require.Contains(t, w.Body.String(), `"changes":["rotate callback authorization"]`)
	require.NotContains(t, w.Body.String(), "private")
}

func TestWebhookApplyOpensNoOuterSession(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(strings.Replace(webhookBody, "%s", "webhook.apply", 1)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	var applied native.WebhookConfig
	controlServer{
		connect: func(context.Context, native.Options) (controlClient, error) {
			t.Fatal("webhook.apply opened a throwaway session")
			return nil, nil
		},
		applyWebhook: func(_ context.Context, options native.Options, config native.WebhookConfig) (*native.WebhookPlan, error) {
			require.NotNil(t, options.Trace)
			applied = config
			return &native.WebhookPlan{Configured: true, Create: 2, Delete: 1, Links: []native.WebhookLink{{EventID: 42, CameraID: 459264}},
				Changes: []string{"create callback target", "add camera 459264", "remove legacy log-only link for camera 459264"}}, nil
		},
	}.serve(w, r)
	require.Equal(t, 200, w.Code)
	require.Equal(t, "owner", applied.OwnerID)
	require.JSONEq(t, `{"configured":true,"verified":true,"create":2,"update":0,"delete":1,"links":[{"eventId":42,"cameraId":459264}],"changes":["create callback target","add camera 459264","remove legacy log-only link for camera 459264"]}`, w.Body.String())
}

func TestWebhookApplyErrorsAreCodedAndSecretFree(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&native.Error{Code: native.CodeConfigRejected, Phase: "webhook", Message: "recorder rejected"}, 422, "config_rejected"},
		{&native.Error{Code: native.CodeConfigConflict, Phase: "webhook", Message: "edited"}, 409, "config_conflict"},
		{errors.New("failed Bearer private /hook?token=private"), 500, "internal"},
	} {
		r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(strings.Replace(webhookBody, "%s", "webhook.apply", 1)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		controlServer{applyWebhook: func(context.Context, native.Options, native.WebhookConfig) (*native.WebhookPlan, error) {
			return nil, tc.err
		}}.serve(w, r)
		require.Equal(t, tc.status, w.Code)
		require.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
		require.NotContains(t, w.Body.String(), "private")
	}
}
