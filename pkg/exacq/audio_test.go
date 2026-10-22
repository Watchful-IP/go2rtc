package exacq

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"
)

func TestAudioMuLawBinaryOracle(t *testing.T) {
	// Digest of all signed int16 values in ascending order, obtained by running
	// the SDK 1.47.27.1 G711::ULawEncode(short) instructions at 0x3a30c0 through
	// an x86-64 emulator. No SDK code, samples, credentials or binary are needed
	// to run this test. The oracle includes negative saturation and zero edges.
	encoded := make([]byte, 65536)
	for i := -32768; i <= 32767; i++ {
		encoded[i+32768] = audioMuLaw(int16(i))
	}
	want := "90c29de505fb68e766118303bd552a16005dcf810873698bee1d8f3b247ce28c"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != want {
		t.Fatalf("mu-law conversion differs from SDK oracle: %s", got)
	}
}

func testAudioInventory() *Inventory {
	format, enabled := uint8(0), true
	return &Inventory{Devices: []Device{{ID: 459264, Audio: []Audio{{
		ID: 459393, WireID: 459281, Output: true, Enabled: &enabled,
		Format: &format, SampleRate: 8000, SamplesPerBuffer: 160,
	}}}}}
}

func TestAudioPacket(t *testing.T) {
	pcm16 := []byte{0, 0x80, 0xff, 0xff, 0, 0, 0xff, 0x7f}
	now := time.Unix(1700000000, 123456789)
	payload := audioPacket(459281, pcm16, 6, now)
	frame, err := parseMediaPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	if frame.StreamID != 459281 || frame.Seconds != 1700000000 || frame.Milliseconds != 123 || frame.Type != MediaAudio || frame.Flags != 0 || len(frame.Motion) != 0 || frame.Reserved != [16]byte{} {
		t.Fatalf("incorrect talkdown header: %+v", frame)
	}
	if !bytes.Equal(frame.Data, []byte{0, 0x7f, 0xff, 0x80, 0xff, 0xff}) {
		t.Fatalf("incorrect mu-law/padding: %x", frame.Data)
	}
}

func TestSendAudioPacesAndCloses(t *testing.T) {
	client, server := sessionPair(t)
	client.inventory = testAudioInventory()
	// C1310-E output channels report Enabled=false; SDK sending still uses fOutput.
	*client.inventory.Devices[0].Audio[0].Enabled = false
	result := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			p, err := server.readPacket()
			if err != nil {
				result <- err
				return
			}
			if p.Type != packetLive || p.Subtype != 3 {
				result <- errors.New("talkdown is not a LIVE/STREAM record")
				return
			}
			frame, err := parseMediaPacket(p.Payload)
			if err != nil || frame.StreamID != 459281 || frame.Type != MediaAudio || len(frame.Data) != 160 || !bytes.Equal(frame.Data, bytes.Repeat([]byte{0xff}, 160)) {
				result <- errors.New("incorrect talkdown media")
				return
			}
		}
		if _, err := server.readPacket(); err == nil {
			result <- errors.New("talkdown session remained open")
			return
		}
		result <- nil
	}()
	started := time.Now()
	// 321 samples requires three 160-sample packets, including final padding.
	if err := client.SendAudio(context.Background(), 459393, make([]byte, 642), 8000); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 59*time.Millisecond {
		t.Fatalf("audio was not paced through its final padded buffer: %v", elapsed)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSendAudioCancellation(t *testing.T) {
	client, server := sessionPair(t)
	client.inventory = testAudioInventory()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := make(chan error, 1)
	go func() {
		_, err := server.readPacket()
		cancel()
		read <- err
	}()
	err := client.SendAudio(ctx, 459393, make([]byte, 8000*2), 8000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

func TestSendAudioServerRejection(t *testing.T) {
	client, server := sessionPair(t)
	client.inventory = testAudioInventory()
	read := make(chan error, 1)
	go func() {
		if _, err := server.readPacket(); err != nil {
			read <- err
			return
		}
		read <- writePacket(server.conn, &packet{Type: packetLive, Subtype: 2, Status: 1})
	}()
	err := client.SendAudio(context.Background(), 459393, make([]byte, 8000*2), 8000)
	if e := AsError(err); e == nil || e.Code != CodeStreamUnavailable || e.RecorderStatus == nil || *e.RecorderStatus != 1 {
		t.Fatalf("server rejection: %v", err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

func TestSendAudioRekey(t *testing.T) {
	client, server := sessionPair(t)
	client.inventory = testAudioInventory()
	result := make(chan error, 1)
	go func() {
		// Complete a rekey between paced buffers. The sender must drain key
		// control packets and use the replacement key for its remaining audio.
		if _, err := server.readPacket(); err != nil {
			result <- err
			return
		}
		const prime, base, secret = 2147483647, 7, 11
		peer := new(big.Int).Exp(big.NewInt(base), big.NewInt(secret), big.NewInt(prime))
		challenge := make([]byte, 24)
		binary.LittleEndian.PutUint64(challenge, base)
		binary.LittleEndian.PutUint64(challenge[8:], prime)
		binary.LittleEndian.PutUint64(challenge[16:], peer.Uint64())
		if err := server.sendPlain(packetKey, 1, challenge); err != nil {
			result <- err
			return
		}
		response, err := server.readPacket()
		if err != nil {
			result <- err
			return
		}
		if response.Type != packetKey || response.Subtype != 2 || len(response.Payload) != 24 {
			result <- errors.New("missing talkdown rekey response")
			return
		}
		shared := new(big.Int).Exp(new(big.Int).SetUint64(binary.LittleEndian.Uint64(response.Payload[16:])), big.NewInt(secret), big.NewInt(prime))
		key := make([]byte, 16)
		binary.LittleEndian.PutUint64(key, shared.Uint64())
		copy(key[8:], key[:8])
		if err = server.sendPlain(packetKey, 3, nil); err != nil {
			result <- err
			return
		}
		server.txKey, server.rxKey = key, key
		for i := 0; i < 2; i++ {
			p, err := server.readPacket()
			if err != nil {
				result <- err
				return
			}
			if p.Type != packetLive || p.Subtype != 3 || len(p.Payload) != 192 {
				result <- errors.New("incorrect talkdown after rekey")
				return
			}
		}
		result <- nil
	}()
	if err := client.SendAudio(context.Background(), 459393, make([]byte, 960), 8000); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestAudioOutputValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		bytes  int
		rate   int
		change func(*Audio)
	}{
		{"empty", 0, 8000, nil}, {"odd", 1, 8000, nil},
		{"oversize", 480002, 8000, nil}, {"frequency", 320, 16000, nil},
		{"input", 320, 8000, func(a *Audio) { a.Output = false }},
		{"format", 320, 8000, func(a *Audio) { *a.Format = 4 }},
		{"missingFormat", 320, 8000, func(a *Audio) { a.Format = nil }},
		{"buffer", 320, 8000, func(a *Audio) { a.SamplesPerBuffer = 0 }},
		{"bigBuffer", 320, 8000, func(a *Audio) { a.SamplesPerBuffer = 8001 }},
		{"wireID", 320, 8000, func(a *Audio) { a.WireID = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Client{inventory: testAudioInventory()}
			if test.change != nil {
				test.change(&c.inventory.Devices[0].Audio[0])
			}
			if _, err := c.audioOutput(459393, test.bytes, test.rate); err == nil {
				t.Fatal("invalid talkdown accepted")
			}
		})
	}
}

func TestAudioPayload(t *testing.T) {
	aac := []byte{0, 0x10, 0, 0x18, 0x21, 0x10, 0x56, 0xff}
	got, err := audioPayload(4, aac)
	if err != nil || !bytes.Equal(got, aac[4:7]) {
		t.Fatalf("AAC AU extraction: %x %v", got, err)
	}
	for _, payload := range [][]byte{nil, {0}, {0, 0x10}, {0, 0x20, 0, 8, 1}, {0, 0x10, 0, 0}, {0, 0x10, 0xff, 0xf8, 1}} {
		if _, err := audioPayload(4, payload); err == nil {
			t.Fatalf("accepted invalid AAC AU: %x", payload)
		}
	}
	if _, err := audioPayload(6, []byte{1}); err == nil {
		t.Fatal("accepted odd PCM16")
	}
	if _, err := audioPayload(1, []byte{1}); err == nil {
		t.Fatal("accepted unimplemented ADPCM")
	}
}

func FuzzAudioPayload(f *testing.F) {
	f.Add(uint8(4), []byte{0, 0x10, 0, 8, 0x21})
	f.Add(uint8(0), []byte{0xff})
	f.Fuzz(func(t *testing.T, format uint8, data []byte) {
		out, err := audioPayload(format, data)
		if err == nil && (len(out) == 0 || len(out) > len(data)) {
			t.Fatal("invalid elementary audio size")
		}
	})
}

func TestAudioPacketPCMEndian(t *testing.T) {
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 1024)
	if got := audioPacket(1, data, 1, time.Unix(1, 0))[32]; got != 0xcd {
		t.Fatalf("PCM16 input byte order: %x", got)
	}
}
