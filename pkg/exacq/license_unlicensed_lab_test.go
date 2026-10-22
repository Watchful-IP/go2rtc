package exacq

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"
)

func TestLabUnlicensedStatusShape(t *testing.T) {
	address, credentials := os.Getenv("EXACQ_AUTHLAB_ADDRESS"), os.Getenv("EXACQ_AUTHLAB_CREDENTIALS")
	if address == "" || credentials == "" || os.Getenv("EXACQ_AUTHLAB_ISOLATED") != "1" {
		t.Skip("requires explicitly isolated unlicensed recorder")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("requires loopback-only isolated recorder")
	}
	data, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal("cannot read private credentials")
	}
	var auth struct{ Username, Password string }
	if json.Unmarshal(data, &auth) != nil || auth.Username == "" || auth.Password == "" {
		t.Fatal("invalid private credentials")
	}
	clear(data)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, Options{Address: address, Username: auth.Username, Password: auth.Password, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Inventory().Devices) != 0 || len(c.Inventory().Cameras) != 0 {
		t.Fatal("requires empty isolated recorder")
	}
	if err = c.sendPacket(packetStatus, 1, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		packet, err := c.readNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if packet.Type != packetStatus || packet.Subtype != 2 {
			continue
		}
		root, err := parseConfigXML(packet.Payload)
		if err != nil {
			t.Fatal(err)
		}
		nodes := root.child("Systems").children("License")
		if len(nodes) == 0 {
			continue
		}
		if len(nodes) != 1 {
			t.Fatal("ambiguous license")
		}
		p := configParser{}
		values := make(map[string]uint32)
		for _, name := range []string{"IsBadLicense", "Trial", "IsRestricted", "Enterprise", "EnterpriseEx", "MaxIPCameras", "TotalIPCameras", "AnalogCameras", "Expiration", "Time", "Unlicensed", "IsUnlicensed"} {
			if nodes[0].has(name) {
				values[name] = p.number(nodes[0], name, 2147483647, true)
			}
		}
		if p.err != nil {
			t.Fatal(p.err)
		}
		t.Logf("allowlisted license numeric attributes=%v", values)
		return
	}
	t.Fatal("no license-bearing status")
}
