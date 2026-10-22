package exacq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLabTransport(t *testing.T) {
	address := os.Getenv("EXACQ_TEST_ADDRESS")
	if address == "" {
		t.Skip("set EXACQ_TEST_ADDRESS for the authorized recorder lab")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := openTransport(ctx, Options{Address: address})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("native transport established; auth mechanism bitmask=%d", c.discovery.NegotiateMechanisms)
}

func TestLabLogin(t *testing.T) {
	camera := uint32(459264)
	if v := os.Getenv("EXACQ_TEST_CAMERA"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			t.Fatal("invalid lab camera ID")
		}
		camera = uint32(n)
	}
	address, credentials := os.Getenv("EXACQ_TEST_ADDRESS"), os.Getenv("EXACQ_TEST_CREDENTIALS")
	if address == "" || credentials == "" {
		t.Skip("set lab address and private credential file")
	}
	data, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Fields []struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		} `json:"fields"`
	}
	if err = json.Unmarshal(data, &item); err != nil {
		t.Fatal("invalid credential file")
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := openTransport(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.login(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("native authenticated login succeeded")
	if path := os.Getenv("EXACQ_TEST_CONFIG_OUT"); path != "" {
		if err = c.sendPacket(packetConfig, 1, nil); err != nil {
			t.Fatal(err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		found := false
		for i := 0; i < 20; i++ {
			p, e := c.readPacket()
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("post-login packet %d/%d status=%d bytes=%d", p.Type, p.Subtype, p.Status, len(p.Payload))
			if p.Type == packetConfig {
				if e = os.WriteFile(path, p.Payload, 0600); e != nil {
					t.Fatal(e)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatal("configuration response not received")
		}
	}
	if path := os.Getenv("EXACQ_TEST_LIVE_OUT"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err = c.sendPacket(packetLive, 1, liveRequest([]uint32{camera}, true)); err != nil {
			t.Fatal(err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(6 * time.Second))
		frames, size, timed := 0, 0, 0
		for {
			p, err := c.readPacket()
			if err != nil {
				var timeout net.Error
				if errors.As(err, &timeout) && timeout.Timeout() {
					break
				}
				t.Fatal(err)
			}
			if p.Type != packetLive || p.Subtype != 3 {
				t.Logf("live control %d/%d status=%d bytes=%d", p.Type, p.Subtype, p.Status, len(p.Payload))
				continue
			}
			media, err := parseMediaPacket(p.Payload)
			if err != nil {
				t.Fatal(err)
			}
			frames++
			size += len(media.Data)
			if !media.Timestamp().IsZero() {
				timed++
			}
			if frames < 6 || (timed < 3 && timed > 0) {
				t.Logf("frame wire=%d type=%x flags=%x time=%d.%03d size=%d", media.StreamID, media.Type, media.Flags, media.Seconds, media.Milliseconds, len(media.Data))
			}
			if _, err = file.Write(media.Data); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("native live frames=%d timed=%d bytes=%d", frames, timed, size)
		if timed == 0 {
			t.Fatal("no timed media")
		}
	}
}

func TestLabNativeMedia(t *testing.T) {
	camera := uint32(459264)
	if v := os.Getenv("EXACQ_TEST_CAMERA"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			t.Fatal("invalid lab camera ID")
		}
		camera = uint32(n)
	}
	address, credentials := os.Getenv("EXACQ_TEST_ADDRESS"), os.Getenv("EXACQ_TEST_CREDENTIALS")
	if address == "" || credentials == "" {
		t.Skip("set lab address and private credential file")
	}
	data, err := os.ReadFile(credentials)
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
		t.Fatal("invalid credential file")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Connect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("inventory devices=%d cameras=%d streams=%d", len(c.Inventory().Devices), len(c.Inventory().Cameras), len(c.Inventory().Streams))
	if err = c.StartLive(camera); err != nil {
		t.Fatal(err)
	}
	var last time.Time
	for i := 0; i < 200; i++ {
		frame, err := c.ReadMedia(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !frame.Timestamp().IsZero() {
			last = frame.Timestamp()
			break
		}
	}
	_ = c.Close()
	if last.IsZero() {
		t.Fatal("no timed media")
	}
	c, err = Connect(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	frames, err := c.Search(ctx, camera, last.Add(-20*time.Second), last.Add(-10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded search frames=%d", len(frames))
	if dir := os.Getenv("EXACQ_TEST_CAPTURE_DIR"); dir != "" {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for i, frame := range frames {
			b, _ := json.Marshal(struct {
				Stream      uint32
				Seconds     uint32
				MS          uint16
				Type, Flags uint8
				Data        []byte
			}{frame.StreamID, frame.Seconds, frame.Milliseconds, frame.Type, frame.Flags, frame.Data})
			if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("frame-%04d.json", i)), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
