package exacq

import (
	"encoding/json"
	"strings"
	"testing"
)

func webhookTestConfig() WebhookConfig {
	return WebhookConfig{OwnerID: testMotionOwner, CameraIDs: []uint32{459264}, URL: "https://receiver.invalid/publish/owner?key=a%26b", Authorization: "Bearer private<&token"}
}

func webhookTestInventory(t *testing.T, config WebhookConfig, configured bool) *Inventory {
	t.Helper()
	body := `<eDVR><Modules><Module Name="EventPI" ID="655360"/></Modules><Webhooks ID="655360" Methods="POST" SendIntervalSec="5"/>`
	if configured {
		u, err := webhookURL(config)
		if err != nil {
			t.Fatal(err)
		}
		target := string(webhookTargetXML(655360, config, u, false))
		body = strings.Replace(body, `<Webhooks ID="655360" Methods="POST" SendIntervalSec="5"/>`, strings.TrimSuffix(strings.TrimPrefix(target, `<eDVR Delta="1">`), `</eDVR>`), 1)
		body = strings.Replace(body, `<Webhooks ID="655360">`, `<Webhooks ID="655360" Methods="POST" SendIntervalSec="5">`, 1)
		for i, camera := range config.CameraIDs {
			link := string(webhookLinkXML(655360, config, camera, uint32(i+7)))
			body += strings.TrimSuffix(strings.TrimPrefix(link, `<eDVR Delta="1">`), `</eDVR>`)
		}
	}
	inv, err := ParseConfig([]byte(body + `</eDVR>`))
	if err != nil {
		t.Fatal(err)
	}
	inv.Cameras = []Camera{{ID: 459264}, {ID: 459520}}
	return inv
}

func TestWebhookPlanCreateVerifyRotateAndRemove(t *testing.T) {
	config := webhookTestConfig()
	inv := webhookTestInventory(t, config, false)
	plan, writes, err := planWebhook(inv, config)
	if err != nil || plan.Create != 2 || len(writes) != 2 || plan.Configured {
		t.Fatalf("create plan failed: %v", err)
	}
	if !strings.Contains(string(writes[0]), `TrustCert="0"`) || strings.Contains(string(writes[0]), "SendInterval") || !strings.Contains(string(writes[1]), `AlarmEnabled="3"`) {
		t.Fatal("unsafe webhook defaults")
	}
	inv = webhookTestInventory(t, config, true)
	plan, writes, err = planWebhook(inv, config)
	if err != nil || !plan.Configured || len(writes) != 0 || len(plan.Links) != 1 || plan.Links[0] != (WebhookLink{EventID: 7, CameraID: 459264}) {
		t.Fatalf("idempotent readback failed: %v", err)
	}
	encoded, err := json.Marshal(struct {
		Inventory *Inventory
		Plan      *WebhookPlan
	}{inv, plan})
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private", "receiver.invalid", "publish/owner", "Authorization"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private webhook configuration leaked")
		}
	}
	config.Authorization = "Bearer rotated"
	plan, writes, err = planWebhook(inv, config)
	if err != nil || plan.Update != 1 || len(writes) != 1 || !strings.Contains(string(writes[0]), "rotated") {
		t.Fatalf("credential rotation failed: %v", err)
	}
	// The recorder keeps an existing header unless it is deleted first, in the same write.
	if !strings.Contains(string(writes[0]), `<Header Name="Authorization" Delete="1"/><Header Name="Authorization" Value=`) {
		t.Fatal("rotation must replace the existing Authorization header")
	}
	if fresh, w, _ := planWebhook(webhookTestInventory(t, config, false), config); fresh.Create == 0 || strings.Contains(string(w[0]), `Delete="1"`) {
		t.Fatal("target creation must not carry a header delete")
	}
	config.CameraIDs = nil
	config.URL, config.Authorization = "", ""
	plan, writes, err = planWebhook(inv, config)
	if err != nil || plan.Delete != 2 || len(writes) != 2 || !strings.Contains(string(writes[0]), `<Event ID="7"`) || !strings.Contains(string(writes[0]), `AssociationGUID=`) || !strings.Contains(string(writes[0]), `Delete="1"`) || !strings.Contains(string(writes[1]), `<Target ID=`) {
		t.Fatalf("scoped cleanup order failed: %v", err)
	}
	delete(inv.webhooks.targets, webhookUUID(config.OwnerID, "target"))
	plan, writes, err = planWebhook(inv, config)
	if err != nil || plan.Delete != 1 || len(writes) != 1 || !strings.Contains(string(writes[0]), `AssociationGUID=`) {
		t.Fatalf("orphaned owned event cleanup failed: %v", err)
	}
}

func TestWebhookPlanPreservesForeignObjectsAndRejectsCollisions(t *testing.T) {
	config := webhookTestConfig()
	inv := webhookTestInventory(t, config, true)
	inv.webhooks.targets["foreign"] = &configNode{name: "Target"}
	inv.EventLinks = append(inv.EventLinks, EventLink{ID: 42, AssociationGUID: "foreign", Source: -1, Target: -1})
	plan, writes, err := planWebhook(inv, config)
	if err != nil || !plan.Configured || len(writes) != 0 {
		t.Fatal("foreign object changed")
	}
	for _, mutate := range []func(*Inventory){
		func(inv *Inventory) { inv.EventLinks[0].Target = -1 },
		func(inv *Inventory) { inv.EventLinks[0].CameraID = 0 },
		func(inv *Inventory) { inv.EventLinks = append(inv.EventLinks, inv.EventLinks[0]) },
		func(inv *Inventory) { inv.EventLinks[0].AssociationGUID = "foreign" },
		func(inv *Inventory) {
			inv.EventLinks = append(inv.EventLinks, EventLink{ID: 99, webhookReferences: []string{webhookUUID(config.OwnerID, "target")}})
		},
		func(inv *Inventory) {
			inv.webhooks.targets[webhookUUID(config.OwnerID, "target")].nodes = append(inv.webhooks.targets[webhookUUID(config.OwnerID, "target")].nodes, &configNode{name: "Route"})
		},
	} {
		inv = webhookTestInventory(t, config, true)
		mutate(inv)
		if _, _, err = planWebhook(inv, config); err == nil {
			t.Fatal("ambiguous owned configuration accepted")
		}
	}
}

func TestWebhookValidationAndDistinctOwnership(t *testing.T) {
	config := webhookTestConfig()
	for _, callback := range []string{"ftp://host/", "http://user:pass@host/", "http://host/#secret", "http:///path", "http://[bad/"} {
		config.URL = callback
		if _, err := webhookURL(config); err == nil || strings.Contains(err.Error(), callback) {
			t.Fatal("unsafe callback URL accepted or disclosed")
		}
	}
	config = webhookTestConfig()
	for _, authorization := range []string{"", "Bearer token\r\nInjected: yes", "Bearer \x00"} {
		config.Authorization = authorization
		if _, err := webhookURL(config); err == nil {
			t.Fatal("unsafe header accepted")
		}
	}
	config = webhookTestConfig()
	inv := webhookTestInventory(t, config, false)
	for _, ids := range [][]uint32{{0}, {123}, {459264, 459264}} {
		config.CameraIDs = ids
		if _, _, err := planWebhook(inv, config); err == nil {
			t.Fatal("invalid camera selection accepted")
		}
	}
	a, b := webhookUUID(testMotionOwner, "target"), webhookUUID(testMotionOwner, "route")
	if a == b || a != webhookUUID(strings.ToUpper(testMotionOwner), "target") || webhookLinkPrefix(testMotionOwner) == strings.ReplaceAll(testMotionOwner, "-", "")[:24] {
		t.Fatal("webhook owner identities are unstable or overlap legacy motion")
	}
}

func TestWebhookRecorderNormalizedReadback(t *testing.T) {
	// Real recorder shape; owner identifiers, endpoint and token replaced.
	data := `<eDVR><Modules><Module ID="655360" Name="EventPI"/></Modules>
<Webhooks ID="655360" Methods="POST" SendIntervalSec="5"><Target ID="09821e2f-1fe0-5131-8409-ea7277c2af6f" Host="https://receiver.invalid" TrustCert="0" DefaultTimeoutMs="5000" Digest="" Secret=""><Route ID="6bc88f53-6176-5be2-adb2-78d3dcc5d098" Path="/callback" Method="POST" TimeoutMs="5000"><Header Name="Authorization" Value="Bearer test"/></Route></Target></Webhooks>
<Events ID="655360"><Event ID="34" Threshold="0" PreAlarm="0" PostAlarm="0" Log="1" AssociationGUID="37dabafa-9cfe-5b59-889e-074500070200"><Devices ID="458752"><Device Number="2"><Video><Input Number="0" Context="0" Motion="1"/></Video></Device></Devices><Action><Webhooks ID="655360"><Target ID="09821e2f-1fe0-5131-8409-ea7277c2af6f"><Route ID="6bc88f53-6176-5be2-adb2-78d3dcc5d098"/></Target></Webhooks></Action><Schedule><Entry FirstHour="0" LastHour="167" AlarmEnabled="3"/></Schedule></Event></Events></eDVR>`
	inv, err := ParseConfig([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	inv.Cameras = []Camera{{ID: 459264}}
	config := WebhookConfig{OwnerID: testMotionOwner, CameraIDs: []uint32{459264}, URL: "https://receiver.invalid/callback", Authorization: "Bearer test"}
	plan, writes, err := planWebhook(inv, config)
	if err != nil || !plan.Configured || len(writes) != 0 || len(plan.Links) != 1 || plan.Links[0].EventID != 34 {
		t.Fatalf("normalized readback rejected: %v", err)
	}
	for _, change := range [][2]string{
		{`PreAlarm="0"`, `PreAlarm="1"`},
		{`</Action>`, `<UnknownAction/></Action>`},
		{`<Input Number="0" Context="0" Motion="1"/>`, `<Input Number="0" Context="0" Motion="1" OtherSource="1"/>`},
		{`<Schedule>`, `<Schedule OtherMode="1">`},
	} {
		broken, parseErr := ParseConfig([]byte(strings.Replace(data, change[0], change[1], 1)))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		broken.Cameras = inv.Cameras
		if _, _, planErr := planWebhook(broken, config); planErr == nil {
			t.Fatal("unknown owned XML action was editable")
		}
	}
}
