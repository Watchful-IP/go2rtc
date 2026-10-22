package exacq

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Diagnosis is a step-by-step connection test. It stops at the first failing
// step so the failure names exactly what to fix.
type Diagnosis struct {
	OK       bool              `json:"ok"`
	Recorder *RecorderIdentity `json:"recorder,omitempty"`
	Steps    []DiagnosisStep   `json:"steps"`
}

type RecorderIdentity struct {
	Serial  string `json:"serial,omitempty"`
	Version string `json:"version,omitempty"`
}

type DiagnosisStep struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms"`
	Detail string `json:"detail,omitempty"`
	// Err is the classified failure; the control API renders it.
	Err error `json:"-"`
}

// Diagnose runs dial, handshake, login, configuration, license and webhook
// capability checks on one short-lived session. It never writes configuration.
func Diagnose(ctx context.Context, options Options) *Diagnosis {
	if options.Timeout <= 0 {
		options.Timeout = 15 * time.Second
	}
	if options.Trace == nil {
		options.Trace = &Trace{}
	}
	d := &Diagnosis{Steps: []DiagnosisStep{}}
	elapsed := func(phase string) int64 {
		for _, p := range options.Trace.Phases() {
			if p.Phase == phase {
				return p.Duration.Milliseconds()
			}
		}
		return 0
	}
	pass := func(step, detail string, ms int64) {
		d.Steps = append(d.Steps, DiagnosisStep{Step: step, OK: true, MS: ms, Detail: detail})
	}
	fail := func(step string, ms int64, err error) *Diagnosis {
		if step != PhaseDial && step != PhaseHandshake {
			options.Trace.Observe(step, time.Now().Add(-time.Duration(ms)*time.Millisecond))
		}
		err = classify(err, step)
		d.Steps = append(d.Steps, DiagnosisStep{Step: step, MS: ms, Err: err})
		return d
	}
	route := "directly"
	if options.ProxyURL != "" {
		route = "through EdgeLink"
	}
	c, err := openTransport(ctx, options)
	if err != nil {
		if e := AsError(err); e == nil || e.Phase == PhaseDial {
			return fail("dial", elapsed(PhaseDial), err)
		}
		pass("dial", fmt.Sprintf("connected %s to %s", route, options.Address), elapsed(PhaseDial))
		return fail("handshake", elapsed(PhaseHandshake), err)
	}
	defer c.Close()
	pass("dial", fmt.Sprintf("connected %s to %s", route, options.Address), elapsed(PhaseDial))
	mechanism := "SPAKE (P-224)"
	if c.discovery.NegotiateMechanisms&2 != 0 {
		mechanism = "SPAKE2+"
	}
	d.Recorder = &RecorderIdentity{Serial: c.discovery.Serial}
	pass("handshake", fmt.Sprintf("exacqVision Server %s answered; login uses %s", c.discovery.Serial, mechanism), elapsed(PhaseHandshake))

	start := time.Now()
	if err = c.login(ctx); err != nil {
		return fail("login", time.Since(start).Milliseconds(), err)
	}
	options.Trace.Observe(PhaseLogin, start)
	pass("login", "authenticated", time.Since(start).Milliseconds())

	start = time.Now()
	inv, err := c.readInventory(ctx)
	if err != nil {
		return fail("configuration", time.Since(start).Milliseconds(), err)
	}
	options.Trace.Observe(PhaseConfiguration, start)
	c.inventoryMu.Lock()
	c.inventory = inv
	c.inventoryMu.Unlock()
	if inv.Server.Serial != "" {
		d.Recorder.Serial = inv.Server.Serial
	}
	d.Recorder.Version = inv.Server.Version
	options.Trace.identify(d.Recorder.Serial, d.Recorder.Version)
	version := inv.Server.Version
	if version == "" {
		version = "unknown version"
	}
	webhooks := "not supported"
	if webhookCapable(inv) {
		webhooks = "supported"
	}
	pass("configuration", fmt.Sprintf("exacqVision %s; %d cameras; webhook POST %s", version, len(inv.Cameras), webhooks), time.Since(start).Milliseconds())

	start = time.Now()
	license, err := c.ReadLicense(ctx)
	if err != nil {
		return fail("license", time.Since(start).Milliseconds(), err)
	}
	if err = licenseUsable(license); err != nil {
		return fail("license", time.Since(start).Milliseconds(), err)
	}
	options.Trace.Observe(PhaseLicense, start)
	pass("license", "licence "+license.Status, time.Since(start).Milliseconds())

	if !webhookCapable(inv) {
		return fail("webhook", 0, errorf(CodeWebhookUnsupported, PhaseWebhook, "recorder (exacqVision %s) does not offer webhook POST notifications; motion events cannot be configured", version))
	}
	pass("webhook", "webhook POST notifications available", 0)
	d.OK = true
	return d
}

func webhookCapable(inv *Inventory) bool {
	return inv != nil && inv.webhooks != nil && inv.webhooks.plugin != 0 && inv.webhooks.plugin == inv.Plugins["EventPI"] &&
		strings.Contains(","+strings.ReplaceAll(inv.webhooks.methods, " ", ",")+",", ",POST,")
}
