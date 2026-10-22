package exacq

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// WebhookConfig provisions recorder-originated POST notifications. Authorization
// is written to the recorder, but never included in inventory or plan JSON.
type WebhookConfig struct {
	OwnerID       string
	CameraIDs     []uint32
	URL           string
	Authorization string
}

type WebhookPlan struct {
	Configured bool          `json:"configured"`
	Create     int           `json:"create"`
	Update     int           `json:"update"`
	Delete     int           `json:"delete"`
	CameraIDs  []uint32      `json:"cameraIds"`
	Links      []WebhookLink `json:"links"`
	// Changes lists each pending (plan) or applied (apply) mutation in
	// secret-free words, in write order.
	Changes []string `json:"changes"`
}

type WebhookLink struct {
	EventID  uint32 `json:"eventId"`
	CameraID uint32 `json:"cameraId"`
}

type webhookInventory struct {
	plugin  uint32
	methods string
	targets map[string]*configNode
}

func (p *configParser) parseWebhooks(root *configNode) {
	nodes := root.children("Webhooks")
	if len(nodes) == 0 {
		return
	}
	if len(nodes) != 1 {
		p.fail(root, "Webhooks")
		return
	}
	node := nodes[0]
	w := &webhookInventory{plugin: p.number(node, "ID", math.MaxUint32, true), methods: node.attr("Methods"), targets: make(map[string]*configNode)}
	for _, target := range node.children("Target") {
		id := target.attr("ID")
		if id == "" || w.targets[id] != nil {
			p.fail(target, "ID")
			return
		}
		w.targets[id] = target
	}
	p.inventory.webhooks = w
}

func (p *configParser) parseWebhookAction(node *configNode, link *EventLink) {
	for _, action := range node.children("Action") {
		for _, hooks := range action.children("Webhooks") {
			for _, target := range hooks.children("Target") {
				link.webhookReferences = append(link.webhookReferences, target.attr("ID"))
			}
		}
	}
	action := node.child("Action")
	w := action.child("Webhooks")
	target := w.child("Target")
	route := target.child("Route")
	// Retain references even for unfamiliar actions so an owned target cannot
	// be deleted or redirected while another event still uses it.
	link.webhookTarget, link.webhookRoute = target.attr("ID"), route.attr("ID")
	if !link.motionSourceSafe || action == nil || w == nil || target == nil || route == nil {
		return
	}
	if len(node.children("Action")) != 1 || len(action.nodes) != 1 || len(w.nodes) != 1 || len(target.nodes) != 1 || len(route.nodes) != 0 ||
		!onlyAttributes(action) || !onlyAttributes(w, "ID") || !onlyAttributes(target, "ID") || !onlyAttributes(route, "ID") ||
		!onlyAttributes(node, "ID", "PreAlarm", "PostAlarm", "Log", "AssociationGUID", "Threshold") {
		return
	}
	if p.number(w, "ID", math.MaxUint32, true) != p.inventory.Plugins["EventPI"] ||
		p.number(node, "PreAlarm", math.MaxUint32, false) != 0 || p.number(node, "PostAlarm", math.MaxUint32, false) != 0 || p.number(node, "Threshold", math.MaxUint32, false) != 0 ||
		link.webhookTarget == "" || link.webhookRoute == "" {
		return
	}
	for _, schedule := range node.children("Schedule") {
		if !onlyAttributes(schedule) || !onlyChildren(schedule, "Entry") {
			return
		}
		for _, entry := range schedule.children("Entry") {
			if len(entry.nodes) != 0 || !onlyAttributes(entry, "FirstHour", "LastHour", "AlarmEnabled") {
				return
			}
		}
	}
	link.Target = 10
}

func webhookUUID(owner, purpose string) string {
	h := sha256.Sum256([]byte("watchful/exacq/webhook/v1/" + strings.ToLower(owner) + "/" + purpose))
	h[6], h[8] = h[6]&15|0x50, h[8]&63|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func webhookLinkPrefix(owner string) string {
	return strings.ReplaceAll(webhookUUID(owner, "events"), "-", "")[:24]
}

func webhookURL(config WebhookConfig) (*url.URL, error) {
	if _, err := motionOwnerPrefix(config.OwnerID); err != nil {
		return nil, errorf(CodeInvalidRequest, PhaseWebhook, "invalid webhook owner UUID")
	}
	if len(config.CameraIDs) == 0 {
		return nil, nil
	}
	u, err := url.Parse(config.URL)
	if err != nil || len(config.URL) > 8192 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errorf(CodeInvalidRequest, PhaseWebhook, "webhook callback must be an absolute http(s) URL without credentials or fragment")
	}
	invalidAuth := errorf(CodeInvalidRequest, PhaseWebhook, "webhook authorization header must be 1-8192 printable ASCII characters")
	if config.Authorization == "" || len(config.Authorization) > 8192 {
		return nil, invalidAuth
	}
	for _, b := range []byte(config.Authorization) {
		if b < 32 || b > 126 {
			return nil, invalidAuth
		}
	}
	return u, nil
}

func webhookEscape(value string) string {
	var out strings.Builder
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

// The recorder ignores a changed Value for an existing header name, so updates
// delete the header in the same write (lab-verified on 26.1: no unauthenticated gap).
func webhookTargetXML(plugin uint32, config WebhookConfig, u *url.URL, update bool) []byte {
	target, route := webhookUUID(config.OwnerID, "target"), webhookUUID(config.OwnerID, "route")
	replace := ""
	if update {
		replace = `<Header Name="Authorization" Delete="1"/>`
	}
	return []byte(fmt.Sprintf(`<eDVR Delta="1"><Webhooks ID="%d"><Target ID="%s" Host="%s" TrustCert="0" DefaultTimeoutMs="5000" Digest="" Secret=""><Route ID="%s" Path="%s" Method="POST" TimeoutMs="5000">%s<Header Name="Authorization" Value="%s"/></Route></Target></Webhooks></eDVR>`,
		plugin, target, webhookEscape(u.Scheme+"://"+u.Host), route, webhookEscape(u.RequestURI()), replace, webhookEscape(config.Authorization)))
}

func webhookLinkXML(plugin uint32, config WebhookConfig, camera uint32, id uint32) []byte {
	guid := motionGUID(webhookLinkPrefix(config.OwnerID), camera)
	return []byte(fmt.Sprintf(`<eDVR Delta="1"><Events ID="%d"><Event ID="%d" PreAlarm="0" PostAlarm="0" Log="1" AssociationGUID="%s" Threshold="0">%s<Action><Webhooks ID="%d"><Target ID="%s"><Route ID="%s"/></Target></Webhooks></Action><Schedule><Entry FirstHour="0" LastHour="167" AlarmEnabled="3"/></Schedule></Event></Events></eDVR>`,
		plugin, id, guid, motionSourceXML(camera), plugin, webhookUUID(config.OwnerID, "target"), webhookUUID(config.OwnerID, "route")))
}
