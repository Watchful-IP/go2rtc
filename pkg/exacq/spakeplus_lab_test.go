package exacq

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"
)

// The target must be an isolated recorder with no registered cameras. This test
// performs one bad-password login, so it must never use a site's live account.
func TestLabSpakePlusRecorder(t *testing.T) {
	address, credentials := os.Getenv("EXACQ_AUTHLAB_ADDRESS"), os.Getenv("EXACQ_AUTHLAB_CREDENTIALS")
	if address == "" || credentials == "" || os.Getenv("EXACQ_AUTHLAB_ISOLATED") != "1" {
		t.Skip("requires an explicitly isolated recorder with strengthened passwords")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("auth lab requires a loopback-only tunnel")
	}
	data, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal("cannot read private lab credentials")
	}
	var auth struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.Unmarshal(data, &auth) != nil || auth.Username == "" || auth.Password == "" {
		t.Fatal("invalid private lab credentials")
	}
	clear(data)
	options := Options{Address: address, Username: auth.Username, Password: auth.Password, Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var loginMS []int64
	for attempt := 0; attempt < 3; attempt++ {
		start := time.Now()
		c, err := Connect(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		if c.discovery.NegotiateMechanisms != 3 {
			c.Close()
			t.Fatal("recorder did not advertise both mechanisms")
		}
		if len(c.Inventory().Devices) != 0 || len(c.Inventory().Cameras) != 0 {
			c.Close()
			t.Fatal("auth lab must have no registered devices or cameras")
		}
		trace := &healthTraceConn{Conn: c.conn, client: c, phase: "authlab-status"}
		c.conn = trace
		if _, err = c.RefreshStatus(ctx); err != nil {
			for _, record := range trace.trace {
				if record.LicenseCount > 0 {
					t.Logf("license fields=%v capacity=%v", record.LicenseFields, record.Capacity)
				}
			}
			c.Close()
			t.Fatal(err)
		}
		trace.phase = "authlab-license"
		license, err := c.ReadLicense(ctx)
		if err != nil {
			for _, record := range trace.trace {
				if record.LicenseCount > 0 {
					t.Logf("license fields=%v capacity=%v", record.LicenseFields, record.Capacity)
				}
			}
			c.Close()
			t.Fatal(err)
		}
		if license.Status != "unlicensed" {
			c.Close()
			t.Fatal("auth lab must not carry a licence")
		}
		loginMS = append(loginMS, time.Since(start).Milliseconds())
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	wrong := options
	wrong.Password += "-wrong-auth-lab-only"
	c, err := Connect(ctx, wrong)
	if err == nil {
		c.Close()
		t.Fatal("bad password was accepted")
	}
	// A fresh successful login distinguishes rejection from an unusable server.
	c, err = Connect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if c.discovery.NegotiateMechanisms != 3 {
		c.Close()
		t.Fatal("authentication mechanism changed")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	evidence := map[string]any{"advertisedMechanisms": 3, "selectedMechanism": 1,
		"successfulLogins": 4, "badPasswordRejected": true, "authenticatedConfiguration": true,
		"statusAndLicense": true, "devices": 0, "license": "unlicensed", "loginAndReadMs": loginMS}
	if path := os.Getenv("EXACQ_AUTHLAB_EVIDENCE"); path != "" {
		encoded, _ := json.MarshalIndent(evidence, "", "  ")
		if err = os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("real recorder SPAKE2+ passed: four logins, status/config/license, one bad-password rejection; login/read ms=%v", loginMS)
}
