package exacq

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRefreshStatusSkipsUnsolicitedPartialReports(t *testing.T) {
	data, err := os.ReadFile("testdata/status.xml")
	if err != nil {
		t.Fatal(err)
	}
	license := strings.TrimSuffix(strings.TrimPrefix(licenseFixture, "<eDVR>"), "</eDVR>")
	full := strings.Replace(string(data), "<eDVR>", "<eDVR>"+license, 1)
	client, server := sessionPair(t)
	before := &Inventory{Cameras: []Camera{{ID: 459264}}}
	client.inventory = before
	done := make(chan error, 1)
	go func() {
		if _, err := server.readPacket(); err != nil {
			done <- err
			return
		}
		for _, body := range []string{
			`<eDVR><Servers><Server/></Servers></eDVR>`,
			`<eDVR><Analytics/></eDVR>`,
			`<eDVR><Users/></eDVR>`,
			full,
		} {
			if err := server.sendPacket(packetStatus, 2, []byte(body)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	status, err := client.RefreshStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Connections[459264] || len(status.Storage) != 1 || status.MemoryThrottling == nil {
		t.Fatal("partial status substituted for complete health")
	}
	if before.Cameras[0].Connected != nil || client.Inventory().Cameras[0].Connected == nil || !*client.Inventory().Cameras[0].Connected {
		t.Fatal("full status did not publish an immutable inventory snapshot")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestParseStatusFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/status.xml")
	if err != nil {
		t.Fatal(err)
	}
	status, err := ParseStatus(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint32{459264, 459520, 459776, 4784640} {
		if !status.Connections[id] {
			t.Fatalf("device %d missing connected state", id)
		}
	}
	if len(status.Storage) != 1 || len(status.Storage[0].Drives) != 1 || status.Storage[0].Drives[0].FreeGB != 30 || status.Storage[0].Drives[0].Alarm != 0 {
		t.Fatal("storage status mismatch")
	}
	if status.MemoryThrottling == nil || *status.MemoryThrottling {
		t.Fatal("throttling status mismatch")
	}
	if len(status.Cameras) != 4 || status.Cameras[0].Motion == nil || !*status.Cameras[0].Motion || status.Cameras[0].RecordIdle == nil || *status.Cameras[0].RecordIdle {
		t.Fatal("merged camera status mismatch")
	}
}

func TestParseStatusSequenceAndUnknown(t *testing.T) {
	inv := &Inventory{Streams: []Stream{{ID: 459269, DeviceID: 459264, Encoder: 0, Channel: 1}}}
	data := []byte(`<eDVR><Devices ID="458752"><Device Number="2"><Codec><Encoder Number="0"><Channel Number="1" Connected="0"/></Encoder></Codec></Device><Device Number="3"/></Devices></eDVR>`)
	status, err := ParseStatus(data, inv)
	if err != nil {
		t.Fatal(err)
	}
	if value, found := status.Connections[459269]; !found || value {
		t.Fatal("sequence status mapping mismatch")
	}
	if _, found := status.Connections[459520]; found {
		t.Fatal("invented unknown connection state")
	}
	if _, err = ParseStatus([]byte(`<eDVR><Devices ID="458752"><Device Number="2" Connected="unknown"/></Devices></eDVR>`), inv); err == nil {
		t.Fatal("accepted malformed state")
	}
}

func TestLabRefreshStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := controlLabConnect(t, ctx)
	status, err := c.RefreshStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connected := 0
	for _, camera := range c.Inventory().Cameras {
		if camera.Connected != nil && *camera.Connected {
			connected++
		}
	}
	t.Logf("native status query completed: connections=%d connected_cameras=%d storage=%d", len(status.Connections), connected, len(status.Storage))
}

func FuzzParseStatus(f *testing.F) {
	f.Add([]byte(`<eDVR/>`))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = ParseStatus(data, nil) })
}
