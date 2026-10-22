package exacq

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestLabSearchPackets(t *testing.T) {
	if os.Getenv("EXACQ_TEST_SEARCH_PACKETS") != "1" {
		t.Skip("explicit packet observation only")
	}
	u := producerLabURL(t)
	source, err := ParseURL(u.String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Connect(ctx, source.options())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.sendPacket(packetStored, 1, searchRequest(source.Camera, time.UnixMilli(1792543920000), time.UnixMilli(1792543930000))); err != nil {
		t.Fatal(err)
	}
	count := 0
	var first, last int64
	kinds := map[byte]int{}
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
		p, e := c.readPacket()
		if e != nil {
			var timeout net.Error
			if errors.As(e, &timeout) && timeout.Timeout() {
				break
			}
			t.Fatal(e)
		}
		if p.Type == packetStored && p.Subtype == 3 {
			f, e := parseMediaPacket(p.Payload)
			if e != nil {
				t.Fatal(e)
			}
			count++
			if len(f.Data) == 0 {
				t.Logf("empty media raw=%x", p.Payload)
			}
			kinds[f.Type]++
			last = f.Timestamp().UnixMilli()
			if count == 1 {
				first = last
			}
			if count < 4 || count > 170 {
				t.Logf("frame=%d stream=%d kind=%d time=%d bytes=%d", count, f.StreamID, f.Type, last, len(f.Data))
			}
		} else {
			t.Logf("packet=%d/%d status=%d body=%q", p.Type, p.Subtype, p.Status, p.Payload)
		}
	}
	t.Logf("count=%d first=%d last=%d kinds=%v", count, first, last, kinds)
}
