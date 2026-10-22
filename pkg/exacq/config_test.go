package exacq

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestParseConfigRecorderFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/config.xml")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Devices) != 2 || len(inv.Cameras) != 1 || len(inv.Streams) != 1 {
		t.Fatalf("wrong inventory counts: %d/%d/%d", len(inv.Devices), len(inv.Cameras), len(inv.Streams))
	}
	if inv.Plugins["EventPI"] != 655360 || inv.Plugins["Core"] != 0x80000000 {
		t.Fatal("module IDs changed")
	}
	camera, stream := inv.Cameras[0], inv.Streams[0]
	if camera.ID != 459264 || camera.DeviceID != 459264 || camera.Connected != nil {
		t.Fatalf("wrong camera: %+v", camera)
	}
	if stream.ID != 459264 || stream.WireID != 459264 || stream.ParentCameraID != camera.ID || stream.Format != 5 || stream.Width != 1920 || stream.Height != 1080 || stream.Framerate != 15 || !stream.Enabled {
		t.Fatalf("wrong stream: %+v", stream)
	}
	for _, device := range inv.Devices {
		if len(device.Audio) != 2 {
			t.Fatal("audio inventory missing")
		}
		input, output := device.Audio[0], device.Audio[1]
		if input.DeviceID != device.ID || output.DeviceID != device.ID || output.ID != device.ID|129 || !output.Output || input.Output || output.Format == nil || *output.Format != 0 || output.SampleRate != 8000 {
			t.Fatalf("wrong device audio: %+v", device.Audio)
		}
	}
}

func TestParseConfigMappingsAndSecrets(t *testing.T) {
	data := []byte(`<?xml ?><eDVR><Unknown password="secret"/><Devices ID="458752"><Device Number="2" Name="Camera" username="secret" password="secret"><Video><Input Number="0"><OSD Name="Gate &amp; yard"/></Input><Input Number="4"/></Video><Codec><Encoder Number="0"><Channel Number="0" Standard="5" Width="1920" Height="1080" Framerate="29.97" ID="123" Enabled="1"/></Encoder></Codec><Context Number="1"><Codec><Encoder Number="0"><Channel Number="0" Standard="6" Width="640" Height="360" Framerate="10" ID="124" Enabled="1"/></Encoder></Codec></Context></Device><Device Number="4"><Codec><Encoder Number="0"><Sequence Pages="2"><Entry Position="7" Input="5"/><Entry Position="2" Input="3"/></Sequence><Channel Number="0" Standard="5" Width="720" Height="480" Framerate="15"/><Channel Number="1" Standard="5" Width="720" Height="480" Framerate="15"/></Encoder></Codec></Device></Devices><Groups><Group Name="Outdoor"><Devices ID="458752"><Device Number="2"><Video><Input Number="0"/></Video></Device></Devices><Group Name="Gate"/></Group></Groups></eDVR>`)
	inv, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Cameras[0].Name != "Gate & yard" {
		t.Fatal("OSD name lost")
	}
	if inv.Streams[0].WireID != 123 || inv.Streams[1].WireID != 124 || inv.Streams[1].ID != 0x10070200 || inv.Streams[1].ParentCameraID != 459264 || inv.Streams[1].Format != 6 {
		t.Fatal("context mapping lost")
	}
	if inv.Streams[2].ID != 0x70403 || inv.Streams[3].ID != 0x70405 {
		t.Fatal("sequence input ordering lost")
	}
	if inv.Groups[0].ID != "Outdoor_gr" || inv.Groups[1].ID != "Outdoor_grGate_gr" || inv.Groups[0].CameraIDs[0] != 459264 {
		t.Fatal("group mapping changed")
	}
	encoded, _ := json.Marshal(inv)
	if bytes.Contains(encoded, []byte("secret")) || bytes.Contains(encoded, []byte("password")) {
		t.Fatal("secret retained in inventory")
	}
}

func TestParseConfigRejectsMalformed(t *testing.T) {
	for _, input := range []string{
		``, `<other/>`, `<eDVR/><eDVR/>`, `<eDVR>`, `text<eDVR/>`, `<eDVR a="1" a="2"/>`,
		`<!DOCTYPE eDVR><eDVR/>`, `<eDVR xmlns="urn:example"/>`,
		`<eDVR><Devices ID="458753"/></eDVR>`,
		`<eDVR><Devices ID="458752"><Device Number="256"/></Devices></eDVR>`,
		`<eDVR><Devices ID="458752"><Device Number="secret"/></Devices></eDVR>`,
		`<eDVR><Devices ID="458752"><Device Number="0" Enabled="secret"/></Devices></eDVR>`,
		`<eDVR><Devices ID="458752"><Device Number="0"><Codec><Encoder Number="0"><Channel Number="0" Standard="5" Width="1" Height="1" Framerate="NaN"/></Encoder></Codec></Device></Devices></eDVR>`,
		`<eDVR>` + strings.Repeat(`<Unknown>`, maxConfigDepth) + strings.Repeat(`</Unknown>`, maxConfigDepth) + `</eDVR>`,
	} {
		_, err := ParseConfig([]byte(input))
		if err == nil {
			t.Fatal("accepted malformed configuration")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error leaks attribute contents")
		}
	}
	if _, err := ParseConfig(make([]byte, maxConfigBytes+1)); err == nil {
		t.Fatal("accepted oversized configuration")
	}
}

// Optional local-only evidence. Never commit the recorder's original config:
// Device attributes contain plaintext camera credentials.
func TestLabConfigParse(t *testing.T) {
	path := os.Getenv("EXACQ_TEST_CONFIG_FILE")
	if path == "" {
		t.Skip("set a private captured configuration path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed captured configuration: devices=%d cameras=%d streams=%d groups=%d", len(inv.Devices), len(inv.Cameras), len(inv.Streams), len(inv.Groups))
}

func FuzzParseConfig(f *testing.F) {
	f.Add([]byte(`<eDVR/>`))
	data, err := os.ReadFile("testdata/config.xml")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = ParseConfig(data) })
}
