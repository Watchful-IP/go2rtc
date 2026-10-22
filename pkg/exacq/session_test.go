package exacq

import (
	"context"
	"encoding/binary"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

func sessionPair(t *testing.T) (*Client, *Client) {
	t.Helper()
	local, remote := net.Pipe()
	a := &Client{conn: local, options: Options{Timeout: time.Second}, txKey: make([]byte, 16), rxKey: make([]byte, 16)}
	b := &Client{conn: remote, options: Options{Timeout: time.Second}, txKey: make([]byte, 16), rxKey: make([]byte, 16)}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestSearchCountAndGaps(t *testing.T) {
	client, server := sessionPair(t)
	done := make(chan error, 1)
	go func() {
		p, err := server.readPacket()
		if err != nil {
			done <- err
			return
		}
		if p.Type != packetStored || p.Subtype != 1 {
			done <- errors.New("wrong request")
			return
		}
		body := []byte(`<eDVR><Devices ID="458752"><Device Number="2"><Video><Input Number="0"><Result Start="1700000000" End="1700000010" Count="2" StreamID="123" Standard="5" Width="1920" Height="1080"/></Input></Video></Device></Devices></eDVR>`)
		if err = server.sendPacket(packetStored, 2, body); err != nil {
			done <- err
			return
		}
		for _, sec := range []uint32{1700000000, 1700000007} {
			frame := make([]byte, 36)
			binary.LittleEndian.PutUint32(frame, 123)
			binary.LittleEndian.PutUint32(frame[4:], sec)
			frame[11] = 1
			copy(frame[32:], []byte{0, 0, 1, 0x65})
			if err = server.sendPacket(packetStored, 3, frame); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	frames, err := client.Search(context.Background(), 459264, time.Unix(1700000001, 0), time.Unix(1700000010, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[1].Timestamp().Sub(frames[0].Timestamp()) != 7*time.Second {
		t.Fatal("preroll or recording gap lost")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEmptySearchAndMalformedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fails      bool
	}{
		{"empty", `<eDVR/>`, false},
		{"malformed", `<eDVR`, true},
		{"wrongRoot", `<Error/>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := sessionPair(t)
			done := make(chan error, 1)
			go func() {
				if _, err := server.readPacket(); err != nil {
					done <- err
					return
				}
				done <- server.sendPacket(packetStored, 2, []byte(tc.body))
			}()
			frames, err := client.Search(context.Background(), 459264, time.Unix(1700000000, 0), time.Unix(1700000010, 0))
			if (err != nil) != tc.fails {
				t.Fatalf("unexpected result %v", err)
			}
			if len(frames) != 0 {
				t.Fatal("unexpected media")
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadMediaCancellation(t *testing.T) {
	client, _ := sessionPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.ReadMedia(ctx); result <- err }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read ignored cancellation")
	}
}

func TestSearchBoundaryBeforeNextMetadata(t *testing.T) {
	client, server := sessionPair(t)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			if _, err := server.readPacket(); err != nil {
				done <- err
				return
			}
			if i == 1 {
				marker := make([]byte, streamHeaderSize)
				binary.LittleEndian.PutUint32(marker, 0x50000)
				if err := server.sendPacket(packetStored, 3, marker); err != nil {
					done <- err
					return
				}
			}
			if err := server.sendPacket(packetStored, 2, []byte(`<eDVR/>`)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 2; i++ {
		if _, err := client.Search(context.Background(), 459264, time.Unix(1700000000, 0), time.Unix(1700000010, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, streamHeaderSize)
	binary.LittleEndian.PutUint32(marker, 0x50000)
	for _, index := range []int{4, 8, 10, 11, 12, 16, 31} {
		marker[index] = 1
		if isStoredBoundary(marker) {
			t.Fatal("accepted nonempty media as boundary")
		}
		marker[index] = 0
	}
}

func TestReadMediaOperationDeadline(t *testing.T) {
	client, server := sessionPair(t)
	client.options.Timeout = 80 * time.Millisecond
	go func() {
		for {
			if err := server.sendPacket(packetDiscover, 2, nil); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	start := time.Now()
	if _, err := client.ReadMedia(context.Background()); err == nil {
		t.Fatal("idle controls extended media wait indefinitely")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("operation deadline extended by control packets")
	}
}

func TestSessionRekey(t *testing.T) {
	client, server := sessionPair(t)
	done := make(chan error, 1)
	go func() {
		const prime, base, secret = 2147483647, 7, 11
		peer := new(big.Int).Exp(big.NewInt(base), big.NewInt(secret), big.NewInt(prime))
		challenge := make([]byte, 24)
		binary.LittleEndian.PutUint64(challenge, base)
		binary.LittleEndian.PutUint64(challenge[8:], prime)
		binary.LittleEndian.PutUint64(challenge[16:], peer.Uint64())
		if err := server.sendPlain(packetKey, 1, challenge); err != nil {
			done <- err
			return
		}
		response, err := server.readPacket()
		if err != nil {
			done <- err
			return
		}
		if response.Type != packetKey || response.Subtype != 2 || len(response.Payload) != 24 {
			done <- errors.New("invalid rekey response")
			return
		}
		shared := new(big.Int).Exp(new(big.Int).SetUint64(binary.LittleEndian.Uint64(response.Payload[16:])), big.NewInt(secret), big.NewInt(prime))
		key := make([]byte, 16)
		binary.LittleEndian.PutUint64(key, shared.Uint64())
		copy(key[8:], key[:8])
		if err = server.sendPlain(packetKey, 3, nil); err != nil {
			done <- err
			return
		}
		server.txKey = key
		server.rxKey = key
		done <- server.sendPacket(packetStatus, 2, []byte(`<eDVR/>`))
	}()
	p, err := client.readNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != packetStatus || string(p.Payload) != `<eDVR/>` {
		t.Fatal("rekey failed to decode next packet")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
