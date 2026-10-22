package exacq

import (
	"fmt"
	"net/url"
	"strings"
)

// PlanWebhook compares one owner's desired callback with the current snapshot.
// It exposes identifiers, counts and reasons only; callback credentials stay private.
func (c *Client) PlanWebhook(config WebhookConfig) (*WebhookPlan, error) {
	plan, _, err := planWebhook(c.Inventory(), config)
	return plan, err
}

// planWebhook returns the plan and its writes in a safe order: target first,
// webhook links, this owner's legacy log-only links, then target removal last.
func planWebhook(inv *Inventory, config WebhookConfig) (*WebhookPlan, [][]byte, error) {
	u, err := webhookURL(config)
	if err != nil {
		return nil, nil, err
	}
	if !webhookCapable(inv) {
		version := "unknown version"
		if inv != nil && inv.Server.Version != "" {
			version = inv.Server.Version
		}
		return nil, nil, errorf(CodeWebhookUnsupported, PhaseWebhook, "recorder (exacqVision %s) does not offer webhook POST notifications; upgrade to a server with Notifications > Webhooks", version)
	}
	if len(config.CameraIDs) > 1024 {
		return nil, nil, errorf(CodeInvalidRequest, PhaseWebhook, "too many webhook cameras (maximum 1024)")
	}
	available := make(map[uint32]bool)
	for _, camera := range inv.Cameras {
		available[camera.ID] = true
	}
	wanted := make(map[uint32]bool)
	for _, id := range config.CameraIDs {
		if id == 0 || wanted[id] {
			return nil, nil, errorf(CodeInvalidRequest, PhaseWebhook, "webhook configuration has a zero or duplicate camera %d", id)
		}
		if !available[id] {
			return nil, nil, errorf(CodeCameraNotFound, PhaseWebhook, "camera %d is not on this recorder", id)
		}
		wanted[id] = true
	}
	plan := &WebhookPlan{CameraIDs: append([]uint32{}, config.CameraIDs...), Links: []WebhookLink{}, Changes: []string{}}
	var writes [][]byte
	change := func(data []byte, format string, args ...any) {
		writes = append(writes, data)
		plan.Changes = append(plan.Changes, fmt.Sprintf(format, args...))
	}
	plugin := inv.webhooks.plugin
	targetID, routeID := webhookUUID(config.OwnerID, "target"), webhookUUID(config.OwnerID, "route")
	target := inv.webhooks.targets[targetID]
	if target != nil && !ownedWebhookTarget(target, routeID) {
		return nil, nil, errorf(CodeConfigConflict, PhaseWebhook, "the callback target this integration manages was edited on the recorder; delete it under Notifications > Webhooks and retry")
	}
	if len(wanted) > 0 {
		switch reason := webhookTargetDrift(target, config, u); {
		case target == nil:
			change(webhookTargetXML(plugin, config, u, false), "create callback target")
			plan.Create++
		case reason != "":
			change(webhookTargetXML(plugin, config, u, true), "%s", reason)
			plan.Update++
		}
	}
	existing := make(map[uint32]EventLink)
	prefix := webhookLinkPrefix(config.OwnerID)
	for _, link := range inv.EventLinks {
		ownedGUID := strings.HasPrefix(strings.ToLower(strings.ReplaceAll(link.AssociationGUID, "-", "")), prefix)
		referencesTarget := false
		for _, reference := range link.webhookReferences {
			referencesTarget = referencesTarget || reference == targetID
		}
		if !referencesTarget && !ownedGUID {
			continue
		}
		if !ownedGUID || !strings.EqualFold(link.AssociationGUID, motionGUID(prefix, link.CameraID)) || link.Source != 5 || link.Target != 10 || link.webhookTarget != targetID || link.webhookRoute != routeID {
			return nil, nil, errorf(CodeConfigConflict, PhaseWebhook, "event link %d uses this integration's callback but was edited on the recorder; delete it in exacqVision Client and retry", link.ID)
		}
		if _, found := existing[link.CameraID]; found {
			return nil, nil, errorf(CodeConfigConflict, PhaseWebhook, "recorder has duplicate managed webhook links for camera %d; delete the extra event link in exacqVision Client and retry", link.CameraID)
		}
		existing[link.CameraID] = link
		if !wanted[link.CameraID] {
			id := link.ID
			change(motionChangeXML(plugin, motionChange{id: &id, guid: link.AssociationGUID, remove: true}), "remove camera %d", link.CameraID)
			plan.Delete++
		}
	}
	for _, camera := range config.CameraIDs {
		link, found := existing[camera]
		correct := found && link.Log
		for _, hour := range link.Schedule {
			correct = correct && hour == 3
		}
		if found {
			plan.Links = append(plan.Links, WebhookLink{EventID: link.ID, CameraID: camera})
		}
		if correct {
			continue
		}
		if found {
			change(webhookLinkXML(plugin, config, camera, link.ID), "repair event link for camera %d", camera)
			plan.Update++
		} else {
			change(webhookLinkXML(plugin, config, camera, 0), "add camera %d", camera)
			plan.Create++
		}
	}
	// Earlier releases polled a log-only link per camera. Push replaces it.
	legacy, _ := motionOwnerPrefix(config.OwnerID)
	for _, link := range inv.EventLinks {
		if !strings.HasPrefix(strings.ToLower(strings.ReplaceAll(link.AssociationGUID, "-", "")), legacy) {
			continue
		}
		if link.Source != 5 || link.Target != 0 || !strings.EqualFold(link.AssociationGUID, motionGUID(legacy, link.CameraID)) {
			return nil, nil, errorf(CodeConfigConflict, PhaseWebhook, "event link %d carries this integration's legacy identifier but was edited on the recorder; delete it in exacqVision Client and retry", link.ID)
		}
		id := link.ID
		change(motionChangeXML(plugin, motionChange{id: &id, guid: link.AssociationGUID, remove: true}), "remove legacy log-only link for camera %d", link.CameraID)
		plan.Delete++
	}
	if len(wanted) == 0 && target != nil {
		change([]byte(fmt.Sprintf(`<eDVR Delta="1"><Webhooks ID="%d"><Target ID="%s" Delete="1"/></Webhooks></eDVR>`, plugin, targetID)), "remove callback target")
		plan.Delete++
	}
	plan.Configured = len(writes) == 0
	return plan, writes, nil
}

func ownedWebhookTarget(target *configNode, routeID string) bool {
	if !onlyAttributes(target, "ID", "Host", "TrustCert", "DefaultTimeoutMs", "Digest", "Secret") || len(target.nodes) != 1 {
		return false
	}
	route := target.child("Route")
	if route == nil || route.attr("ID") != routeID || !onlyAttributes(route, "ID", "Path", "Method", "TimeoutMs") || len(route.nodes) != 1 {
		return false
	}
	header := route.child("Header")
	return header != nil && len(header.nodes) == 0 && onlyAttributes(header, "Name", "Value") && header.attr("Name") == "Authorization"
}

// webhookTargetDrift names the first difference between an existing target
// and the desired one, or "" when it already matches.
func webhookTargetDrift(target *configNode, config WebhookConfig, u *url.URL) string {
	if target == nil || u == nil {
		return ""
	}
	route := target.child("Route")
	switch {
	case target.attr("Host") != u.Scheme+"://"+u.Host || route.attr("Path") != u.RequestURI():
		return "update callback URL"
	case route.child("Header").attr("Value") != config.Authorization:
		return "rotate callback authorization"
	case (target.attr("TrustCert") != "0" && target.attr("TrustCert") != "false") || target.attr("DefaultTimeoutMs") != "5000" ||
		target.attr("Digest") != "" || target.attr("Secret") != "" || route.attr("Method") != "POST" || route.attr("TimeoutMs") != "5000":
		return "repair callback target settings"
	}
	return ""
}
