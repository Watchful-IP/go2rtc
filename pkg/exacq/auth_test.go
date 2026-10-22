package exacq

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestLoginKeyTransition(t *testing.T) {
	for _, validProof := range []bool{true, false} {
		t.Run(fmt.Sprint(validProof), func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()
			initialKey := bytes.Repeat([]byte{0x42}, 16)
			client := &Client{conn: local, options: Options{Username: "test-user", Password: "test-password", Timeout: time.Second}, discovery: Discovery{Serial: "LAB-RECORDER", NegotiateMechanisms: 1}, txKey: initialKey, rxKey: initialKey}
			defer client.Close()
			server := &Client{conn: remote, options: Options{Timeout: time.Second}, txKey: initialKey, rxKey: initialKey}
			result := make(chan error, 1)
			go func() { result <- respondLogin(server, validProof) }()
			err := client.login(context.Background())
			if validProof && err != nil {
				t.Fatal(err)
			}
			if !validProof && err == nil {
				t.Fatal("accepted incorrect server proof")
			}
			if err = <-result; err != nil {
				t.Fatal(err)
			}
			if validProof && (bytes.Equal(client.txKey, initialKey) || !bytes.Equal(client.txKey, client.rxKey) || client.loginKeyPending) {
				t.Fatal("did not complete receive-key transition")
			}
		})
	}
}

// The synthetic server computes the reciprocal SPAKE result and verifies the
// client's proof before replying using the new key. This exercises the exact
// old-key/new-key packet boundary and the complete login read/write loop.
func respondLogin(server *Client, validProof bool) error {
	p, err := server.readPacket()
	if err != nil {
		return err
	}
	if p.Type != packetLogin || p.Subtype != 5 {
		return errors.New("unexpected initial packet")
	}
	_, clientPoint, username, err := decodeSpakePacket(p.Payload)
	if err != nil {
		return err
	}
	curve := elliptic.P224()
	passwordHash := sha256.Sum224([]byte("test-password"))
	private := bytes.Repeat([]byte{0x63}, 28)
	n, _ := hex.DecodeString(spake224N)
	nx, ny, _ := decodeP224(n)
	nx, ny = curve.ScalarMult(nx, ny, passwordHash[:])
	yx, yy := curve.ScalarBaseMult(private)
	yx, yy = curve.Add(yx, yy, nx, ny)
	peer := encodeP224(yx, yy)
	m, _ := hex.DecodeString(spake224M)
	mx, my, _ := decodeP224(m)
	mx, my = curve.ScalarMult(mx, my, passwordHash[:])
	my.Sub(curve.Params().P, my)
	x, y, err := decodeP224(clientPoint)
	if err != nil {
		return err
	}
	x, y = curve.Add(x, y, mx, my)
	x, y = curve.ScalarMult(x, y, private)
	shared := encodeP224(x, y)
	proof := pakeHash([]byte("LAB-RECORDER"), clientPoint, peer, passwordHash[:], shared)
	if !validProof {
		proof[0] ^= 1
	}
	if err = server.sendPacket(packetLogin, 5, encodeSpakePacket(0, peer, proof[:])); err != nil {
		return err
	}
	if !validProof {
		_, err = server.readPacket()
		if err == nil {
			return errors.New("client disclosed proof after bad server proof")
		}
		return nil
	}
	p, err = server.readPacket()
	if err != nil {
		return err
	}
	_, clientProof, echoedProof, err := decodeSpakePacket(p.Payload)
	want := pakeHash(username, clientPoint, peer, passwordHash[:], shared)
	if err != nil || p.Type != packetLogin || p.Subtype != 5 || !bytes.Equal(clientProof, want[:]) || !bytes.Equal(echoedProof, proof[:]) {
		return errors.New("incorrect client proof")
	}
	key := pakeHash(username, []byte("LAB-RECORDER"), clientPoint, peer, passwordHash[:], shared)
	server.txKey = key[:16]
	return server.sendPacket(packetLogin, 2, []byte(`<eDVR/>`))
}

func TestLoginCancellation(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	client := &Client{conn: local, options: Options{Timeout: time.Second}, discovery: Discovery{NegotiateMechanisms: 1}, txKey: make([]byte, 16), rxKey: make([]byte, 16)}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = readPacket(remote)
		cancel()
	}()
	if err := client.login(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation result: %v", err)
	}
}

func TestLoginRejectsFailureAndPrematureSuccess(t *testing.T) {
	for _, status := range []uint16{0, 7} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()
			client := &Client{conn: local, options: Options{Timeout: time.Second}, discovery: Discovery{NegotiateMechanisms: 1}, txKey: make([]byte, 16), rxKey: make([]byte, 16)}
			defer client.Close()
			serverDone := make(chan error, 1)
			go func() {
				if _, err := readPacket(remote); err != nil {
					serverDone <- err
					return
				}
				serverDone <- writePacket(remote, &packet{Type: packetLogin, Subtype: 2, Status: status})
			}()
			if err := client.login(context.Background()); err == nil {
				t.Fatal("accepted premature or failed acknowledgement")
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPeriodicTransportRekey(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	initial := bytes.Repeat([]byte{0x42}, 16)
	client := &Client{conn: local, options: Options{Timeout: time.Second}, txKey: initial, rxKey: initial}
	defer client.Close()
	server := &Client{conn: remote, options: Options{Timeout: time.Second}, txKey: initial, rxKey: initial}
	serverDone := make(chan error, 1)
	go func() {
		challenge := make([]byte, 24)
		binary.LittleEndian.PutUint64(challenge, 5)
		binary.LittleEndian.PutUint64(challenge[8:], 2147483647)
		binary.LittleEndian.PutUint64(challenge[16:], 15625) // 5^6 mod p
		if err := server.sendPlain(packetKey, 1, challenge); err != nil {
			serverDone <- err
			return
		}
		reply, err := readPacket(remote)
		if err != nil {
			serverDone <- err
			return
		}
		if reply.Type != packetKey || reply.Subtype != 2 || reply.Encryption != 0 || len(reply.Payload) != 24 {
			serverDone <- errors.New("invalid rekey reply")
			return
		}
		public := new(big.Int).SetUint64(binary.LittleEndian.Uint64(reply.Payload[16:]))
		shared := new(big.Int).Exp(public, big.NewInt(6), big.NewInt(2147483647))
		key := make([]byte, 16)
		binary.LittleEndian.PutUint64(key, shared.Uint64())
		copy(key[8:], key[:8])
		if err = server.sendPacket(packetKey, 3, nil); err != nil {
			serverDone <- err
			return
		}
		server.txKey, server.rxKey = key, key
		if err = server.sendPacket(packetStatus, 2, []byte(`<eDVR/>`)); err != nil {
			serverDone <- err
			return
		}
		reply, err = server.readPacket()
		if err == nil && (reply.Type != packetDiscover || reply.Subtype != 1) {
			err = errors.New("incorrect post-rekey request")
		}
		serverDone <- err
	}()
	p, err := client.readNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != packetStatus || string(p.Payload) != `<eDVR/>` || bytes.Equal(client.rxKey, initial) || !bytes.Equal(client.rxKey, client.txKey) {
		t.Fatal("incorrect receive key after rekey acknowledgement")
	}
	if err = client.sendPacket(packetDiscover, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
}
