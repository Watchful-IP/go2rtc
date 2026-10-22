package exacq

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLabControlErrors is read-only: it never writes recorder configuration.
// EXACQ_TEST_ADDRESS=host:port EXACQ_TEST_CREDENTIALS=<1Password item JSON>.
func TestLabControlErrors(t *testing.T) {
	address, path := os.Getenv("EXACQ_TEST_ADDRESS"), os.Getenv("EXACQ_TEST_CREDENTIALS")
	if address == "" || path == "" {
		t.Skip("set authorized lab address and private credential file")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var item struct {
		Fields []struct{ ID, Value string } `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(data, &item))
	var username, password string
	for _, field := range item.Fields {
		switch field.ID {
		case "username":
			username = field.Value
		case "password":
			password = field.Value
		}
	}
	host, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	call := func(action, address, password string) (int, string, string) {
		request := controlRequest{Action: action, Address: address, Username: username, Password: password}
		if action == "webhook.plan" {
			// Plan only: an unused owner shows the full create sequence, no writes.
			request.OwnerID, request.CameraIDs = "00000000-0000-4000-8000-00000000c0de", []uint32{459264}
			request.Webhook = &webhookRequest{URL: "https://receiver.invalid/hook", Authorization: "Bearer lab"}
		}
		body, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/api/exacq", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		controlHandler(w, r)
		text := w.Body.String()
		require.NotContains(t, text, password)
		if action == "health" && w.Code == 200 {
			text = fmt.Sprintf("(%d bytes)", len(text))
		}
		return w.Code, text, w.Header().Get("Server-Timing")
	}
	for _, tc := range []struct{ name, action, address, password string }{
		{"diagnose", "diagnose", address, password},
		{"health", "health", address, password},
		{"webhook plan", "webhook.plan", address, password},
		{"wrong password", "inventory", address, password + "-wrong"},
		{"web service port", "inventory", net.JoinHostPort(host, "180"), password},
		{"closed port", "inventory", net.JoinHostPort(host, "22699"), password},
	} {
		status, body, timing := call(tc.action, tc.address, tc.password)
		t.Logf("%s: HTTP %d %s\n  Server-Timing: %s", tc.name, status, body, timing)
	}
}
