package exacq

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPublicSearchTimeBounds(t *testing.T) {
	for _, tc := range []struct{ start, end time.Time }{
		{time.Unix(-1, 0), time.Unix(1, 0)},
		{time.Unix(math.MaxInt32, 0), time.Unix(math.MaxInt32+1, 0)},
		{time.Unix(math.MaxInt32-1, 0), time.Unix(math.MaxInt32, 1)},
	} {
		client, _ := sessionPair(t)
		if _, err := client.Search(context.Background(), 459264, tc.start, tc.end); err == nil || (!strings.Contains(err.Error(), "search segment") && !strings.Contains(err.Error(), "recorder range")) {
			t.Fatalf("video search time was not rejected before transport: %v", err)
		}
	}
	client, _ := sessionPair(t)
	at := time.Unix(math.MaxInt32-3, 1)
	if _, err := client.Snapshot(context.Background(), 459264, 1280, &at); err == nil || !strings.Contains(err.Error(), "recorder range") {
		t.Fatalf("snapshot three-second window crossed supported recorder time: %v", err)
	}
}

func TestReadInventorySkipsConfigurationDelta(t *testing.T) {
	client, server := sessionPair(t)
	done := make(chan error, 1)
	go func() {
		if _, err := server.readPacket(); err != nil {
			done <- err
			return
		}
		for _, body := range []string{
			`<eDVR Delta="1"><Servers><Server Model="partial"/></Servers></eDVR>`,
			`<eDVR><Servers><Server Model="full"/></Servers><Modules><Module Name="EventPI" ID="655360"/></Modules></eDVR>`,
		} {
			if err := server.sendPacket(packetConfig, 2, []byte(body)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	inv, err := client.readInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Server.Model != "full" || inv.Plugins["EventPI"] != 655360 {
		t.Fatal("unsolicited delta replaced the full inventory")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControlOperationDeadlineAndClose(t *testing.T) {
	for name, run := range map[string]func(*Client) error{
		"inventory": func(c *Client) error { _, err := c.readInventory(context.Background()); return err },
		"status":    func(c *Client) error { _, err := c.RefreshStatus(context.Background()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			client, server := sessionPair(t)
			client.options.Timeout = 80 * time.Millisecond
			client.inventory = &Inventory{Plugins: map[string]uint32{"EventPI": 655360}}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := server.readPacket(); err != nil {
					return
				}
				for {
					if err := server.sendPacket(packetDiscover, 2, []byte(`<eDVR/>`)); err != nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
			start := time.Now()
			if err := run(client); err == nil {
				t.Fatal("control traffic extended operation indefinitely")
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("operation exceeded its fixed deadline")
			}
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("failed operation left its session open")
			}
		})
	}
}

func TestReadInventoryControlByteBudget(t *testing.T) {
	client, server := sessionPair(t)
	// This test must move more than the decoded byte budget through encrypted
	// pipes, including under the race detector. Operation deadline enforcement
	// is tested independently above with an 80 ms limit.
	client.options.Timeout = 20 * time.Second
	server.options.Timeout = 20 * time.Second
	done := make(chan error, 1)
	go func() {
		if _, err := server.readPacket(); err != nil {
			done <- err
			return
		}
		body := make([]byte, maxDecodedSize/2+1)
		for i := 0; i < 2; i++ {
			if err := server.sendPacket(packetDiscover, 2, body); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	_, err := client.readInventory(context.Background())
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("missing cumulative byte limit: %v", err)
	}
	if err = <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMalformedMediaReplyClosesSession(t *testing.T) {
	for _, live := range []bool{false, true} {
		client, server := sessionPair(t)
		done := make(chan error, 1)
		go func() {
			kind, subtype := packetLive, uint32(3)
			if !live {
				if _, err := server.readPacket(); err != nil {
					done <- err
					return
				}
				kind, subtype = packetStored, 2
			}
			done <- server.sendPacket(kind, subtype, []byte("malformed"))
		}()
		var err error
		if live {
			_, err = client.ReadMedia(context.Background())
		} else {
			_, err = client.Search(context.Background(), 459264, time.Unix(100, 0), time.Unix(110, 0))
		}
		if err == nil {
			t.Fatal("malformed media accepted")
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		// A subsequent operation cannot reuse an interrupted packet stream.
		if err = client.conn.SetReadDeadline(time.Now().Add(time.Second)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("malformed response left session reusable")
		}
	}
}
