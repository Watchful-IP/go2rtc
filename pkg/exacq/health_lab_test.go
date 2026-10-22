package exacq

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// This diagnostic retains only transport metadata and allowlisted XML shape.
// It never writes payloads, credentials, device config, MACs or license keys.
type healthTrace struct {
	At              time.Time         `json:"at"`
	Phase           string            `json:"phase"`
	Type            uint32            `json:"type"`
	Subtype         uint32            `json:"subtype"`
	Status          uint16            `json:"status"`
	EncodedBytes    int               `json:"encodedBytes"`
	DecodedBytes    int               `json:"decodedBytes,omitempty"`
	Delta           string            `json:"delta,omitempty"`
	HasSystems      bool              `json:"hasSystems,omitempty"`
	LicenseCount    int               `json:"licenseCount,omitempty"`
	LicenseFields   []string          `json:"licenseFields,omitempty"`
	Capacity        map[string]uint32 `json:"capacity,omitempty"`
	DeviceSections  int               `json:"deviceSections,omitempty"`
	Sections        []string          `json:"sections,omitempty"`
	TransformFailed bool              `json:"transformFailed,omitempty"`
}

type healthTraceConn struct {
	net.Conn
	client *Client
	phase  string
	buffer []byte
	trace  []healthTrace
}

func (c *healthTraceConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	if len(c.trace) >= 512 {
		c.buffer = nil
		return n, err
	}
	c.buffer = append(c.buffer, data[:n]...)
	for len(c.buffer) >= headerSize && len(c.trace) < 512 {
		size := int(binary.LittleEndian.Uint32(c.buffer[:4]))
		if size < headerSize || size > maxPacketSize || len(c.buffer) < size {
			break
		}
		c.record(c.buffer[:size])
		c.buffer = c.buffer[size:]
	}
	if len(c.trace) >= 512 {
		c.buffer = nil
	}
	return n, err
}

func TestHealthTraceStopsBufferingAtCaptureLimit(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	done := make(chan error, 1)
	go func() {
		_, err := remote.Write(make([]byte, 8192))
		_ = remote.Close()
		done <- err
	}()
	trace := &healthTraceConn{Conn: local, trace: make([]healthTrace, 512), buffer: make([]byte, 32)}
	count, err := io.Copy(io.Discard, trace)
	if err != nil || count != 8192 || len(trace.buffer) != 0 || len(trace.trace) != 512 {
		t.Fatal("completed trace retained additional transport data")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func (c *healthTraceConn) record(raw []byte) {
	p, err := readPacket(bytes.NewReader(raw))
	if err != nil {
		return
	}
	entry := healthTrace{At: time.Now().UTC(), Phase: c.phase, Type: p.Type, Subtype: p.Subtype, Status: p.Status, EncodedBytes: len(raw)}
	defer func() { c.trace = append(c.trace, entry) }()
	if p.Type != packetStatus && p.Type != packetConfig {
		return // In particular, never decode or retain the license-key reply.
	}
	if p.Encryption == 1 {
		p.Payload, err = decryptPayload(p.Payload, c.client.rxKey)
		if err != nil {
			entry.TransformFailed = true
			return
		}
	}
	p.Payload, err = decompressPayload(p.Payload, p.Compression)
	if err != nil {
		entry.TransformFailed = true
		return
	}
	defer clear(p.Payload)
	entry.DecodedBytes = len(p.Payload)
	root, err := parseConfigXML(p.Payload)
	if err != nil {
		return
	}
	if delta := root.attr("Delta"); delta == "1" || delta == "0" || delta == "true" || delta == "false" {
		entry.Delta = delta
	}
	entry.HasSystems = root.child("Systems") != nil
	licenses := root.child("Systems").children("License")
	entry.LicenseCount = len(licenses)
	if len(licenses) == 1 {
		for _, name := range []string{"IsBadLicense", "Trial", "IsRestricted", "Enterprise", "MaxIPCameras", "Expiration", "Time"} {
			if licenses[0].has(name) {
				entry.LicenseFields = append(entry.LicenseFields, name)
			}
		}
		entry.Capacity = make(map[string]uint32)
		parser := configParser{}
		for _, name := range []string{"MaxIPCameras", "TotalIPCameras", "AnalogCameras"} {
			if licenses[0].has(name) {
				value := parser.number(licenses[0], name, 65536, true)
				if parser.err == nil {
					entry.Capacity[name] = value
				}
			}
		}
	}
	entry.DeviceSections = len(root.children("Devices"))
	for _, child := range root.nodes {
		entry.Sections = append(entry.Sections, child.name)
	}
}

func TestLabRetainedHealthTrace(t *testing.T) {
	path := os.Getenv("EXACQ_TEST_HEALTH_TRACE")
	if path == "" {
		t.Skip("set private sanitized health trace output path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	checks := 20
	if raw := os.Getenv("EXACQ_TEST_HEALTH_CHECKS"); raw != "" {
		var err error
		checks, err = strconv.Atoi(raw)
		if err != nil || checks < 1 || checks > 20 {
			t.Fatal("health checks must be between 1 and 20")
		}
	}
	c := controlLabConnect(t, ctx)
	c.writeMu.Lock()
	trace := &healthTraceConn{Conn: c.conn, client: c}
	c.conn = trace
	c.writeMu.Unlock()
	defer func() {
		data, err := json.MarshalIndent(trace.trace, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Error(err)
		}
	}()
	for check := 0; check < checks; check++ {
		trace.phase = "status"
		status, err := c.RefreshStatus(ctx)
		if err != nil {
			t.Fatalf("health %d status failed: %v", check+1, err)
		}
		if len(status.Connections) < len(c.Inventory().Devices) || len(status.Storage) == 0 || status.MemoryThrottling == nil {
			t.Fatalf("health %d received incomplete lab status", check+1)
		}
		time.Sleep(500 * time.Millisecond)
		trace.phase = "license"
		license, err := c.ReadLicense(ctx)
		if err != nil {
			t.Fatalf("health %d license failed: %v", check+1, err)
		}
		if license.Status != "trial" {
			t.Fatalf("unexpected lab license classification: %s", license.Status)
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Logf("%d retained-session health checks completed; sanitized packets=%d", checks, len(trace.trace))
}
