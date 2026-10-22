package exacq

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// EventLink describes log-only and webhook motion links. Unknown source/action
// combinations use -1 and are never edited.
type EventLink struct {
	ID                uint32     `json:"id"`
	AssociationGUID   string     `json:"associationGuid"`
	Source            int        `json:"source"`
	CameraID          uint32     `json:"cameraId,omitempty"`
	Target            int        `json:"target"`
	Log               bool       `json:"log"`
	Schedule          [168]uint8 `json:"schedule"`
	webhookTarget     string
	webhookRoute      string
	motionSourceSafe  bool
	webhookReferences []string
}

func (p *configParser) parseEventLinks(root *configNode) {
	for _, events := range root.children("Events") {
		for _, node := range events.children("Event") {
			link := EventLink{ID: p.number(node, "ID", math.MaxUint32, true), AssociationGUID: node.attr("AssociationGUID"),
				Source: -1, Target: -1, Log: p.boolean(node, "Log", false)}
			// Be conservative when classifying links that we might later edit.
			// An unfamiliar child or source attribute may represent another action.
			devices := node.children("Devices")
			if len(devices) == 1 && len(devices[0].nodes) == 1 && devices[0].nodes[0].name == "Device" {
				plugin := p.number(devices[0], "ID", 0x0fff0000, true)
				device := devices[0].nodes[0]
				base := plugin | p.number(device, "Number", 255, true)<<8
				container := device
				if context := device.child("Context"); context != nil && len(device.nodes) == 1 {
					base |= p.number(context, "Number", 15, true) << 28
					container = context
				}
				video := container.child("Video")
				if video != nil && len(video.nodes) == 1 && video.nodes[0].name == "Input" {
					input := video.nodes[0]
					if input.has("Context") {
						base = base&0x0fffffff | p.number(input, "Context", 15, true)<<28
					}
					if p.boolean(input, "Motion", false) {
						link.Source, link.CameraID = 5, base|p.number(input, "Number", 127, true)
						if len(input.nodes) == 0 && onlyAttributes(input, "Number", "Context", "Motion") && onlyAttributes(device, "Number") && onlyAttributes(container, "Number") && len(container.nodes) == 1 && onlyChildren(node, "Devices", "Schedule", "Action") {
							link.motionSourceSafe = true
							if onlyChildren(node, "Devices", "Schedule") {
								link.Target = 0
							}
						}
					}
				}
			}
			for _, entry := range node.child("Schedule").children("Entry") {
				first, last := p.number(entry, "FirstHour", 167, true), p.number(entry, "LastHour", 167, true)
				value := p.number(entry, "AlarmEnabled", 3, true)
				if last < first {
					p.fail(entry, "LastHour")
					continue
				}
				for hour := first; hour <= last; hour++ {
					link.Schedule[hour] = uint8(value)
				}
			}
			p.parseWebhookAction(node, &link)
			p.inventory.EventLinks = append(p.inventory.EventLinks, link)
		}
	}
}

func onlyAttributes(node *configNode, names ...string) bool {
	for attr := range node.attrs {
		found := false
		for _, name := range names {
			if name == attr {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func onlyChildren(node *configNode, names ...string) bool {
	for _, child := range node.nodes {
		found := false
		for _, name := range names {
			if name == child.name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func motionOwnerPrefix(ownerID string) (string, error) {
	invalid := errorf(CodeInvalidRequest, PhaseMotion, "invalid owner UUID")
	if len(ownerID) != 36 || ownerID[8] != '-' || ownerID[13] != '-' || ownerID[18] != '-' || ownerID[23] != '-' {
		return "", invalid
	}
	compact := strings.ReplaceAll(ownerID, "-", "")
	if len(compact) != 32 {
		return "", invalid
	}
	if _, err := hex.DecodeString(compact); err != nil {
		return "", invalid
	}
	return strings.ToLower(compact[:24]), nil
}

func motionGUID(prefix string, camera uint32) string {
	value := fmt.Sprintf("%s%08x", prefix, camera)
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
}

type motionChange struct {
	id     *uint32
	guid   string
	camera uint32
	remove bool
}

func motionSourceXML(camera uint32) string {
	plugin, device, input, context := camera&0x0fff0000, (camera>>8)&255, camera&127, camera>>28
	video := fmt.Sprintf(`<Video><Input Number="%d" Motion="1"/></Video>`, input)
	if context != 0 {
		video = fmt.Sprintf(`<Context Number="%d">%s</Context>`, context, video)
	}
	return fmt.Sprintf(`<Devices ID="%d"><Device Number="%d">%s</Device></Devices>`, plugin, device, video)
}

func motionChangeXML(plugin uint32, change motionChange) []byte {
	id := ` ID="0"`
	if change.id != nil {
		id = fmt.Sprintf(` ID="%d"`, *change.id)
	}
	// guid is either generated from a validated owner UUID or validated to be
	// that exact generated UUID by planMotion; no unchecked text is interpolated.
	attrs := fmt.Sprintf(`%s PreAlarm="0" PostAlarm="0" Log="1" AssociationGUID="%s" Threshold="0"`, id, change.guid)
	content := motionSourceXML(change.camera) + `<Schedule><Entry FirstHour="0" LastHour="167" AlarmEnabled="3"/></Schedule>`
	if change.remove {
		attrs += ` Delete="1"`
		content = ""
	}
	return []byte(fmt.Sprintf(`<eDVR Delta="1"><Events ID="%d"><Event%s>%s</Event></Events></eDVR>`, plugin, attrs, content))
}
