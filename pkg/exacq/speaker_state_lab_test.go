package exacq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

var originalLabDeviceIDs = [...]uint32{459264, 459520, 459776, 4784640}

// TestLabSpeakerEnabledState is a coordinated lab recovery operation, never an
// ordinary test. It changes only Enabled on the known C1310-E speaker. The
// state is deliberately left as requested; the caller owns the hardware window.
func TestLabSpeakerEnabledState(t *testing.T) {
	state := os.Getenv("EXACQ_TEST_SPEAKER_STATE")
	if state == "" {
		t.Skip("requires coordinated speaker state mutation: EXACQ_TEST_SPEAKER_STATE=0|1")
	}
	if state != "0" && state != "1" {
		t.Fatal("EXACQ_TEST_SPEAKER_STATE must be exactly 0 or 1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := controlLabConnect(t, ctx)
	before := c.Inventory()
	body, err := labSpeakerEnabledXML(before, state)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	drainErr := make(chan error, 1)
	go func() {
		defer close(drained)
		for {
			p, err := c.readNext(ctx)
			if err != nil {
				drainErr <- err
				return
			}
			if p.Type == packetConfig && p.Status != 0 {
				drainErr <- fmt.Errorf("speaker state rejected: status %d", p.Status)
				return
			}
		}
	}()
	defer func() { _ = c.Close(); <-drained }()
	// A single narrow delta; never resend during persistence polling.
	if err = c.sendPacket(packetConfig, 3, body); err != nil {
		t.Fatal(err)
	}
	verified := 0
	for verified < 3 {
		fresh, err := Connect(ctx, c.options)
		if err != nil {
			t.Fatal(err)
		}
		inv := fresh.Inventory()
		_ = fresh.Close()
		if _, err = labSpeakerEnabledXML(inv, state); err != nil {
			t.Fatal(err)
		}
		persisted := false
		for _, id := range originalLabDeviceIDs {
			old, current := labDeviceByID(before, id), labDeviceByID(inv, id)
			if old.Name != current.Name || old.Model != current.Model || old.Serial != current.Serial || old.Address != current.Address {
				t.Fatalf("original device %d identity changed", id)
			}
			if id == 459520 {
				persisted = current.Enabled == (state == "1")
			} else if old.Enabled != current.Enabled {
				t.Fatalf("unrelated original device %d enabled state changed", id)
			}
		}
		if persisted {
			verified++
			t.Logf("fresh speaker snapshot %d: device=459520 enabled=%s output=459649 originalDevices=4", verified, state)
		} else {
			verified = 0
		}
		if verified == 3 {
			return
		}
		timer := time.NewTimer(150 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatal("speaker state did not persist before deadline")
		case err := <-drainErr:
			timer.Stop()
			t.Fatal(err)
		case <-timer.C:
		}
	}
}

func labDeviceByID(inv *Inventory, id uint32) *Device {
	for i := range inv.Devices {
		if inv.Devices[i].ID == id {
			return &inv.Devices[i]
		}
	}
	return nil
}

func labSpeakerEnabledXML(inv *Inventory, state string) ([]byte, error) {
	if inv == nil || (state != "0" && state != "1") {
		return nil, errors.New("invalid speaker recovery request")
	}
	for _, id := range originalLabDeviceIDs {
		count := 0
		for _, device := range inv.Devices {
			if device.ID == id {
				count++
			}
		}
		if count != 1 {
			return nil, errors.New("original lab device missing or duplicated")
		}
	}
	speaker := labDeviceByID(inv, 459520)
	if speaker.PluginID != 458752 || speaker.Number != 3 || !strings.Contains(strings.ToUpper(speaker.Model+" "+speaker.Name), "C1310") {
		return nil, errors.New("speaker recovery identity guard failed")
	}
	outputs := 0
	for _, audio := range speaker.Audio {
		if audio.ID == 459649 && audio.DeviceID == 459520 && audio.Output {
			outputs++
		}
	}
	if outputs != 1 {
		return nil, errors.New("speaker recovery output guard failed")
	}
	return []byte(`<eDVR Delta="1"><Devices ID="458752"><Device Number="3" Enabled="` + state + `"/></Devices></eDVR>`), nil
}

func TestLabSpeakerEnabledXMLGuards(t *testing.T) {
	inv := &Inventory{}
	for _, id := range originalLabDeviceIDs {
		inv.Devices = append(inv.Devices, Device{ID: id})
	}
	speaker := labDeviceByID(inv, 459520)
	speaker.PluginID, speaker.Number, speaker.Model = 458752, 3, "AXIS C1310-E"
	speaker.Audio = []Audio{{ID: 459649, DeviceID: 459520, Output: true}}
	for _, state := range []string{"0", "1"} {
		body, err := labSpeakerEnabledXML(inv, state)
		if err != nil || string(body) != `<eDVR Delta="1"><Devices ID="458752"><Device Number="3" Enabled="`+state+`"/></Devices></eDVR>` {
			t.Fatal("incorrect narrow speaker state delta")
		}
	}
	for _, state := range []string{"", "true", "2", `1" Password="x`} {
		if _, err := labSpeakerEnabledXML(inv, state); err == nil {
			t.Fatal("accepted invalid speaker state")
		}
	}
	speaker.Audio[0].Output = false
	if _, err := labSpeakerEnabledXML(inv, "1"); err == nil {
		t.Fatal("accepted wrong audio direction")
	}
	speaker.Audio[0].Output = true
	speaker.Model = "other"
	if _, err := labSpeakerEnabledXML(inv, "1"); err == nil {
		t.Fatal("accepted wrong speaker identity")
	}
}
