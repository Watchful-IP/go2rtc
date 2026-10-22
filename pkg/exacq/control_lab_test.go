package exacq

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This recovery probe deletes only structurally verified links belonging to
// the dedicated lab owner, including duplicates from failed reconciliation.
func TestLabMotionCleanup(t *testing.T) {
	if os.Getenv("EXACQ_TEST_CONTROL_CLEANUP") != "1" {
		t.Skip("requires coordinated dedicated-owner recovery window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c := controlLabConnect(t, ctx)
	const owner = "869d31c4-6435-49ce-81ef-df1cf8106691"
	prefix, _ := motionOwnerPrefix(owner)
	var unrelated []EventLink
	var owned []EventLink
	for _, link := range c.Inventory().EventLinks {
		if !strings.HasPrefix(strings.ReplaceAll(strings.ToLower(link.AssociationGUID), "-", ""), prefix) {
			unrelated = append(unrelated, link)
			continue
		}
		if link.Source != 5 || link.Target != 0 || !strings.EqualFold(link.AssociationGUID, motionGUID(prefix, link.CameraID)) {
			t.Fatal("refusing to remove unrecognized owner-scoped event action")
		}
		owned = append(owned, link)
	}
	go func() {
		for {
			if _, err := c.readNext(ctx); err != nil {
				return
			}
		}
	}()
	for _, link := range owned {
		id := link.ID
		if err := c.sendPacket(packetConfig, 3, motionChangeXML(c.Inventory().Plugins["EventPI"], motionChange{id: &id, guid: link.AssociationGUID, remove: true})); err != nil {
			t.Fatal(err)
		}
		t.Logf("removed dedicated-owner event ID %d", id)
	}
	for check := 0; check < 5; check++ {
		time.Sleep(150 * time.Millisecond)
		fresh, err := Connect(ctx, c.options)
		if err != nil {
			t.Fatal(err)
		}
		links := fresh.Inventory().EventLinks
		_ = fresh.Close()
		if !reflect.DeepEqual(links, unrelated) {
			t.Fatalf("fresh cleanup snapshot differs from preserved unrelated links: count=%d expected=%d", len(links), len(unrelated))
		}
		t.Logf("fresh cleanup snapshot %d: owned=0 unrelated=%d", check+1, len(links))
	}
}

func controlLabConnect(t *testing.T, ctx context.Context) *Client {
	t.Helper()
	address, path := os.Getenv("EXACQ_TEST_ADDRESS"), os.Getenv("EXACQ_TEST_CREDENTIALS")
	if address == "" || path == "" {
		t.Skip("set authorized lab address and private credential file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Fields []struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		} `json:"fields"`
	}
	if json.Unmarshal(data, &item) != nil {
		t.Fatal("invalid private credential file")
	}
	options := Options{Address: address}
	for _, field := range item.Fields {
		switch field.ID {
		case "username":
			options.Username = field.Value
		case "password":
			options.Password = field.Value
		}
	}
	c, err := Connect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLabStatusCapture(t *testing.T) {
	path := os.Getenv("EXACQ_TEST_STATUS_OUT")
	if path == "" {
		t.Skip("set private status output path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := controlLabConnect(t, ctx)
	if err := c.sendPacket(packetStatus, 1, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		p, err := c.readNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.Type == packetStatus && p.Subtype == 2 {
			if p.Status != 0 {
				t.Fatalf("status request rejected: %d", p.Status)
			}
			if err = os.WriteFile(path, p.Payload, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("native status captured: bytes=%d", len(p.Payload))
			return
		}
	}
	t.Fatal("status response not received")
}
