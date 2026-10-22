package exacq

import (
	native "github.com/AlexxIT/go2rtc/pkg/exacq"
)

type webhookRequest struct {
	URL           string `json:"url"`
	Authorization string `json:"authorization"`
}

type webhookPlanner interface {
	PlanWebhook(native.WebhookConfig) (*native.WebhookPlan, error)
}

func (request controlRequest) webhookConfig() native.WebhookConfig {
	config := native.WebhookConfig{OwnerID: request.OwnerID, CameraIDs: request.CameraIDs}
	if request.Webhook != nil {
		config.URL = request.Webhook.URL
		config.Authorization = request.Webhook.Authorization
	}
	return config
}
