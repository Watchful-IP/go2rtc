package exacq

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// Inventory exposes non-secret eDVR configuration. Camera and stream IDs are
// CASIDs; WireID is session-local. Private webhook state is retained only for
// reconciliation and is excluded from JSON.
type Inventory struct {
	Server     ServerInfo        `json:"server"`
	Plugins    map[string]uint32 `json:"plugins"`
	Devices    []Device          `json:"devices"`
	Cameras    []Camera          `json:"cameras"`
	Streams    []Stream          `json:"streams"`
	Groups     []Group           `json:"groups"`
	EventLinks []EventLink       `json:"eventLinks"`
	webhooks   *webhookInventory
}

type ServerInfo struct {
	Model       string `json:"model,omitempty"`
	Serial      string `json:"serial,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	UTCTime     uint32 `json:"utcTime,omitempty"`
	Uptime      uint64 `json:"uptime,omitempty"`
	TimeServer  string `json:"timeServer,omitempty"`
}

type Device struct {
	ID        uint32  `json:"id"`
	PluginID  uint32  `json:"pluginId"`
	Number    uint32  `json:"number"`
	Name      string  `json:"name"`
	Model     string  `json:"model,omitempty"`
	Serial    string  `json:"serial,omitempty"`
	Firmware  string  `json:"firmware,omitempty"`
	Address   string  `json:"address,omitempty"`
	Enabled   bool    `json:"enabled"`
	Connected *bool   `json:"connected,omitempty"`
	Audio     []Audio `json:"audio,omitempty"`
}

type Camera struct {
	ID        uint32 `json:"id"`
	DeviceID  uint32 `json:"deviceId"`
	Context   uint32 `json:"context"`
	Input     uint32 `json:"input"`
	Name      string `json:"name"`
	Connected *bool  `json:"connected,omitempty"`
	Motion    *bool  `json:"motion,omitempty"`
}

type Stream struct {
	ID             uint32  `json:"id"`
	WireID         uint32  `json:"wireId"`
	DeviceID       uint32  `json:"deviceId"`
	ParentCameraID uint32  `json:"parentCameraId"`
	Context        uint32  `json:"context"`
	Encoder        uint32  `json:"encoder"`
	Channel        uint32  `json:"channel"`
	Format         uint32  `json:"format"` // SDK values: JPEG 0, H264 5, H265 6.
	Width          uint32  `json:"width"`
	Height         uint32  `json:"height"`
	Framerate      float64 `json:"framerate"`
	Enabled        bool    `json:"enabled"`
	Connected      *bool   `json:"connected,omitempty"`
}

// Audio inventory describes recorder-owned inputs and outputs. This does not
// imply a streaming or talkdown implementation for the reported format.
type Audio struct {
	ID               uint32 `json:"id"`
	DeviceID         uint32 `json:"deviceId"`
	Input            uint32 `json:"input"`
	Name             string `json:"name"`
	Output           bool   `json:"output"`
	SearchOnly       bool   `json:"searchOnly"`
	Enabled          *bool  `json:"enabled,omitempty"`
	Format           *uint8 `json:"format,omitempty"`
	SampleRate       uint32 `json:"sampleRate,omitempty"`
	BitRate          uint32 `json:"bitRate,omitempty"`
	WireID           uint32 `json:"wireId,omitempty"`
	SamplesPerBuffer uint64 `json:"samplesPerBuffer,omitempty"`
}

type Group struct {
	ID          string   `json:"id"`
	ParentID    string   `json:"parentId,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	CameraIDs   []uint32 `json:"cameraIds,omitempty"`
}

const (
	maxConfigBytes = 32 << 20
	maxConfigNodes = 250000
	maxConfigDepth = 64
)

// ParseConfig reads a complete authenticated configuration, not a status delta.
// Unknown elements are tolerated. Malformed known fields fail the parse without
// including their contents in errors (the input can contain device passwords).
func ParseConfig(data []byte) (*Inventory, error) {
	root, err := parseConfigXML(data)
	if err != nil {
		return nil, err
	}
	return parseConfigRoot(root)
}

func parseConfigRoot(root *configNode) (*Inventory, error) {
	p := configParser{inventory: &Inventory{Plugins: make(map[string]uint32)}}
	for _, server := range root.child("Servers").children("Server") {
		p.inventory.Server = ServerInfo{
			Model: server.attr("Model"), Serial: server.attr("Serial"),
			Description: server.attr("Description"),
			UTCTime:     p.number(server.child("Time"), "UTCTime", math.MaxUint32, false),
			Uptime:      p.integer(server.child("Time"), "Uptime", math.MaxUint64, false),
			TimeServer:  server.child("TimeServer").attr("Server"),
		}
	}
	p.inventory.Server.Version = root.child("Product").attr("Version")
	for _, module := range root.child("Modules").children("Module") {
		if name := module.attr("Name"); name != "" {
			id, err := strconv.ParseInt(module.attr("ID"), 10, 64)
			if err != nil || id < math.MinInt32 || id > math.MaxUint32 {
				p.fail(module, "ID")
			}
			p.inventory.Plugins[name] = uint32(id)
		}
	}
	for _, devices := range root.children("Devices") {
		plugin := p.number(devices, "ID", 0x0fff0000, true)
		if plugin&0xffff != 0 {
			p.fail(devices, "ID")
		}
		for _, element := range devices.children("Device") {
			device := Device{
				PluginID: plugin, Number: p.number(element, "Number", 255, true),
				Name: element.attr("Name"), Model: element.attr("OEM"),
				Serial: element.attr("Serial"), Firmware: element.attr("Firmware"),
				Address: element.attr("ipAddress"), Enabled: p.boolean(element, "Enabled", true),
			}
			device.ID = device.PluginID | device.Number<<8
			if device.Model == "" {
				device.Model = devices.attr("Description")
			}
			p.parseDeviceContext(element, &device, 0)
			for _, context := range element.children("Context") {
				p.parseDeviceContext(context, &device, p.number(context, "Number", 15, true))
			}
			p.inventory.Devices = append(p.inventory.Devices, device)
		}
	}
	for _, groups := range root.children("Groups") {
		p.parseGroups(groups, "")
	}
	p.parseEventLinks(root)
	p.parseWebhooks(root)
	if p.err != nil {
		return nil, p.err
	}
	return p.inventory, nil
}

type configParser struct {
	inventory *Inventory
	err       error
}

func (p *configParser) parseDeviceContext(element *configNode, device *Device, context uint32) {
	base := context<<28 | device.ID
	for _, input := range element.child("Video").children("Input") {
		number := p.number(input, "Number", 127, true)
		camera := Camera{ID: base | number, DeviceID: device.ID, Context: context, Input: number, Name: input.child("OSD").attr("Name")}
		if input.has("Loss") {
			connected := !p.boolean(input, "Loss", false)
			camera.Connected = &connected
		}
		camera.Motion = p.optionalBool(input, "Motion")
		p.inventory.Cameras = append(p.inventory.Cameras, camera)
	}
	for _, input := range element.child("Audio").children("Input") {
		number := p.number(input, "Number", 127, true)
		device.Audio = append(device.Audio, Audio{ID: device.ID | number | 128, DeviceID: device.ID, Input: number,
			Name: input.attr("Name"), Output: p.boolean(input, "Output", false), SearchOnly: p.boolean(input, "SearchOnly", false)})
	}
	for _, encoder := range element.child("Codec").children("Encoder") {
		encoderNumber := p.number(encoder, "Number", 7, true)
		var sequence []uint32
		if node := encoder.child("Sequence"); node != nil {
			positions := make(map[uint32]uint32)
			for _, entry := range node.children("Entry") {
				position := p.number(entry, "Position", math.MaxInt32, true)
				positions[position] = p.number(entry, "Input", 127, true)
			}
			keys := make([]uint32, 0, len(positions))
			for key := range positions {
				keys = append(keys, key)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			sequence = make([]uint32, 0, len(keys))
			for _, key := range keys {
				sequence = append(sequence, positions[key])
			}
		}
		for _, channel := range encoder.children("Channel") {
			channelNumber := p.number(channel, "Number", 127, true)
			input := encoderNumber<<4 + channelNumber
			if encoder.child("Sequence") != nil {
				if int(channelNumber) >= len(sequence) {
					continue
				}
				input = sequence[channelNumber]
			}
			if input > 127 {
				p.fail(channel, "Number")
			}
			id := base | input
			stream := Stream{ID: id, WireID: base | channelNumber, DeviceID: device.ID,
				ParentCameraID: id & 0x0fffff7f, Context: context, Encoder: encoderNumber, Channel: channelNumber,
				Format: p.number(channel, "Standard", math.MaxInt32, true), Width: p.number(channel, "Width", 65535, true),
				Height: p.number(channel, "Height", 65535, true), Framerate: p.decimal(channel, "Framerate", true),
				Enabled: p.boolean(channel, "Enabled", device.Enabled), Connected: p.optionalBool(channel, "Connected")}
			if stream.Framerate > 1000 {
				p.fail(channel, "Framerate")
			}
			if channel.has("ID") {
				stream.WireID = p.number(channel, "ID", math.MaxInt32, true)
			}
			p.inventory.Streams = append(p.inventory.Streams, stream)
		}
		for _, channel := range encoder.children("AudioChannel") {
			number := p.number(channel, "Number", 15, true)
			input := encoderNumber<<4 + number
			index := -1
			for i := range device.Audio {
				if device.Audio[i].Input == input {
					index = i
					break
				}
			}
			if index < 0 {
				device.Audio = append(device.Audio, Audio{ID: device.ID | input | 128, DeviceID: device.ID, Input: input})
				index = len(device.Audio) - 1
			}
			audio := &device.Audio[index]
			audio.Enabled = p.optionalBool(channel, "Enabled")
			if channel.has("Standard") {
				format := uint8(p.number(channel, "Standard", 255, true))
				audio.Format = &format
			}
			audio.SampleRate = p.number(channel, "SampleRate", math.MaxUint32, false)
			audio.BitRate = p.number(channel, "BitRate", math.MaxUint32, false)
			audio.SamplesPerBuffer = p.integer(channel, "BufSizeSamples", math.MaxUint64, false)
			audio.WireID = context<<28 | device.ID | number + 16
			if channel.has("ID") {
				audio.WireID = p.number(channel, "ID", math.MaxInt32, true)
			}
		}
	}
}

func (p *configParser) parseGroups(element *configNode, parent string) {
	for _, node := range element.children("Group") {
		name := node.attr("Name")
		group := Group{ID: parent + name + "_gr", ParentID: parent, Name: name, Description: node.attr("Description")}
		if mapID := node.attr("Map"); mapID != "" {
			group.ID = mapID + "_mp"
		}
		for _, devices := range node.children("Devices") {
			plugin := p.number(devices, "ID", 0x0fff0000, true)
			for _, device := range devices.children("Device") {
				base := plugin | p.number(device, "Number", 255, true)<<8
				add := func(element *configNode, context uint32) {
					for _, input := range element.child("Video").children("Input") {
						group.CameraIDs = append(group.CameraIDs, base|context<<28|p.number(input, "Number", 127, true))
					}
				}
				add(device, 0)
				for _, context := range device.children("Context") {
					add(context, p.number(context, "Number", 15, true))
				}
			}
		}
		p.inventory.Groups = append(p.inventory.Groups, group)
		p.parseGroups(node, group.ID)
	}
}

func (p *configParser) fail(node *configNode, attr string) {
	if p.err == nil {
		p.err = fmt.Errorf("exacq: invalid config %s attribute %s", node.name, attr)
	}
}

func (p *configParser) integer(node *configNode, attr string, max uint64, required bool) uint64 {
	if !node.has(attr) && !required {
		return 0
	}
	value, err := strconv.ParseUint(node.attr(attr), 10, 64)
	if err != nil || value > max {
		p.fail(node, attr)
		return 0
	}
	return value
}

func (p *configParser) number(node *configNode, attr string, max uint32, required bool) uint32 {
	return uint32(p.integer(node, attr, uint64(max), required))
}

func (p *configParser) boolean(node *configNode, attr string, fallback bool) bool {
	if !node.has(attr) {
		return fallback
	}
	switch node.attr(attr) {
	case "1", "true":
		return true
	case "0", "false":
		return false
	default:
		p.fail(node, attr)
		return fallback
	}
}

func (p *configParser) optionalBool(node *configNode, attr string) *bool {
	if !node.has(attr) {
		return nil
	}
	value := p.boolean(node, attr, false)
	return &value
}

func (p *configParser) decimal(node *configNode, attr string, required bool) float64 {
	if !node.has(attr) && !required {
		return 0
	}
	value, err := strconv.ParseFloat(node.attr(attr), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		p.fail(node, attr)
		return 0
	}
	return value
}

type configNode struct {
	name  string
	attrs map[string]string
	nodes []*configNode
}

func (n *configNode) attr(name string) string {
	if n == nil {
		return ""
	}
	return n.attrs[name]
}
func (n *configNode) has(name string) bool {
	if n == nil {
		return false
	}
	_, ok := n.attrs[name]
	return ok
}
func (n *configNode) child(name string) *configNode {
	if n != nil {
		for _, child := range n.nodes {
			if child.name == name {
				return child
			}
		}
	}
	return nil
}
func (n *configNode) children(name string) []*configNode {
	var result []*configNode
	if n != nil {
		for _, child := range n.nodes {
			if child.name == name {
				result = append(result, child)
			}
		}
	}
	return result
}

func parseConfigXML(data []byte) (*configNode, error) {
	if len(data) > maxConfigBytes {
		return nil, errors.New("exacq: config exceeds size limit")
	}
	data = bytes.TrimPrefix(data, []byte("<?xml ?>"))
	data = bytes.TrimRight(data, "\x00")
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *configNode
	var stack []*configNode
	nodes := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("exacq: malformed config XML")
		}
		switch element := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > maxConfigNodes || len(stack) >= maxConfigDepth || len(element.Attr) > 128 {
				return nil, errors.New("exacq: config complexity limit exceeded")
			}
			if element.Name.Space != "" {
				return nil, errors.New("exacq: config namespaces unsupported")
			}
			node := &configNode{name: element.Name.Local, attrs: make(map[string]string, len(element.Attr))}
			for _, attr := range element.Attr {
				if attr.Name.Space != "" || node.has(attr.Name.Local) {
					return nil, errors.New("exacq: invalid config XML attribute")
				}
				node.attrs[attr.Name.Local] = attr.Value
			}
			if len(stack) == 0 {
				if root != nil || node.name != "eDVR" {
					return nil, errors.New("exacq: expected one eDVR config root")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.nodes = append(parent.nodes, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 && len(bytes.TrimSpace(element)) != 0 {
				return nil, errors.New("exacq: text outside config root")
			}
		case xml.Directive:
			return nil, errors.New("exacq: config XML directives unsupported")
		}
	}
	if root == nil || len(stack) != 0 {
		return nil, errors.New("exacq: incomplete config XML")
	}
	return root, nil
}
