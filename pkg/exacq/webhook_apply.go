package exacq

import (
	"context"
	"time"
)

// configSession is the slice of a Client that applying configuration needs.
type configSession interface {
	Inventory() *Inventory
	writeConfig([]byte) error
	drainConfig(context.Context) error
	Close() error
}

type sessionConnector func(context.Context, Options) (configSession, error)

func connectConfigSession(ctx context.Context, options Options) (configSession, error) {
	c, err := Connect(ctx, options)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) writeConfig(data []byte) error { return c.sendPacket(packetConfig, 3, data) }

// drainConfig reads acknowledgements and broadcasts so large inventories do
// not fill the recorder's socket and stall writes. It returns on the first
// rejected configuration packet or read failure.
func (c *Client) drainConfig(ctx context.Context) error {
	for {
		p, err := c.readNext(ctx)
		if err != nil {
			return err
		}
		if p.Type == packetConfig && p.Status != 0 {
			return configRejected(PhaseWebhook, p.Status)
		}
	}
}

// webhookVerifyDelay is the wait before verification readback attempt n.
// Each readback is a full login and configuration read, so back off.
var webhookVerifyDelay = func(attempt int) time.Duration {
	return min(200*time.Millisecond<<attempt, 2*time.Second)
}

// ApplyWebhook reconciles one owner's callback using its own worker session,
// sending each planned mutation once and verifying with fresh authenticated
// readbacks. Empty CameraIDs removes this owner's links and target, preserving
// other owners. The result lists the applied changes and the verified links.
func ApplyWebhook(ctx context.Context, options Options, config WebhookConfig) (*WebhookPlan, error) {
	plan, _, err := applyWebhook(ctx, options, config, connectConfigSession)
	return plan, err
}

// ConfigureWebhook applies config and refreshes this client's inventory.
func (c *Client) ConfigureWebhook(ctx context.Context, config WebhookConfig) error {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	_, fresh, err := applyWebhook(ctx, c.options, config, connectConfigSession)
	if fresh != nil {
		c.inventoryMu.Lock()
		c.inventory = fresh
		c.inventoryMu.Unlock()
	}
	return err
}

func applyWebhook(ctx context.Context, options Options, config WebhookConfig, connect sessionConnector) (result *WebhookPlan, fresh *Inventory, err error) {
	defer func() { err = classify(err, PhaseWebhook) }()
	if _, err = webhookURL(config); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	release, err := lockWebhookOwner(ctx, options, config.OwnerID)
	if err != nil {
		if AsError(err) == nil {
			err = wrapf(err, CodeBusy, PhaseWebhook, "another configuration for this recorder owner is still running; retry shortly")
		}
		return nil, nil, err
	}
	defer release()
	worker, err := connect(ctx, options)
	if err != nil {
		return nil, nil, err
	}
	defer worker.Close()
	plan, writes, err := planWebhook(worker.Inventory(), config)
	if err != nil {
		return nil, nil, err
	}
	if plan.Configured {
		return plan, worker.Inventory(), nil
	}
	stop := context.AfterFunc(ctx, func() { _ = worker.Close() })
	defer stop()
	drainErr := make(chan error, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainErr <- worker.drainConfig(ctx)
	}()
	defer func() { _ = worker.Close(); <-drained }()
	targetID := webhookUUID(config.OwnerID, "target")
	deferTarget := len(config.CameraIDs) == 0 && worker.Inventory().webhooks.targets[targetID] != nil
	if deferTarget {
		writes = writes[:len(writes)-1]
	}
	for _, data := range writes {
		if err = worker.writeConfig(data); err != nil {
			return nil, nil, err
		}
	}
	// An acknowledgement may precede persistence. Only a new session's full
	// snapshot establishes success; verification never retries a create.
	for attempt := 0; ; attempt++ {
		timer := time.NewTimer(webhookVerifyDelay(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, wrapf(ctx.Err(), CodeConfigNotPersisted, PhaseWebhook, "recorder accepted the webhook configuration but a fresh login did not show it within 15s; retry")
		case err = <-drainErr:
			timer.Stop()
			return nil, nil, err
		case <-timer.C:
		}
		verify, connectErr := connect(ctx, options)
		if connectErr != nil {
			return nil, nil, connectErr
		}
		fresh = verify.Inventory()
		_ = verify.Close()
		remaining, remainingWrites, planErr := planWebhook(fresh, config)
		if planErr != nil {
			return nil, nil, planErr
		}
		if remaining.Configured {
			plan.Configured, plan.Links = true, remaining.Links
			return plan, fresh, nil
		}
		// Do not orphan event links when the recorder rejects their deletion.
		// Fresh readback must show only the target remains before removing it.
		if deferTarget && len(remainingWrites) == 1 && fresh.webhooks.targets[targetID] != nil {
			if err = worker.writeConfig(remainingWrites[0]); err != nil {
				return nil, nil, err
			}
			deferTarget = false
		}
	}
}
