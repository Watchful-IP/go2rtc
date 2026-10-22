package exacq

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// These synthetic values were calculated independently using affine P-224
// arithmetic and Python hashlib, rather than by this package's helpers.
func TestSpake224Fixture(t *testing.T) {
	const clientPoint = "e276cbf2fb01f6546b39783fa76d3d356bdfcf1a2cd7bce0616628969c5947d5388e6719af21d77ab586be87221f170d9ac60a1edc6dba41"
	const peerPoint = "4b775b032a7d34b9193518ada5b76cd78e907a988c0292fbe06daf273c4a815eacede6f479353d419a6ebb8bf6620b22a3f0e793cce1c3da"
	const clientProof = "03e7e325421f78edb3598a1520f89da086edaea9e6007dfd3d9b2a0e642f8837"
	for _, tc := range []struct{ serial, proof, key string }{
		{"LAB-RECORDER", "4cd703a7f04ef452d73774e1aeba3cad4a22fa492dc7199876bba94dc61807f5", "77280889126063ace4d1bb157b60c693"},
		{"", "fc0cb615a5c87ea887158453b93edf83298fc6e6c14380f8332c1bf924198ec8", "fc0cb615a5c87ea887158453b93edf83"},
	} {
		t.Run(tc.serial, func(t *testing.T) {
			private := make([]byte, 28)
			for i := range private {
				private[i] = byte(i + 1)
			}
			s, err := newSpake224("test-user", "test-password", tc.serial, bytes.NewReader(private))
			if err != nil {
				t.Fatal(err)
			}
			mechanism, point, username, err := decodeSpakePacket(s.start())
			if err != nil || mechanism != 0 || hex.EncodeToString(point) != clientPoint || string(username) != "test-user" {
				t.Fatalf("unexpected client initiation: mechanism=%d point=%x username=%q err=%v", mechanism, point, username, err)
			}
			peer, _ := hex.DecodeString(peerPoint)
			proof, _ := hex.DecodeString(tc.proof)
			reply, key, err := s.finish(encodeSpakePacket(0, peer, proof))
			if err != nil {
				t.Fatal(err)
			}
			mechanism, first, second, err := decodeSpakePacket(reply)
			if err != nil || mechanism != 0 || hex.EncodeToString(first) != clientProof || !bytes.Equal(second, proof) || hex.EncodeToString(key) != tc.key {
				t.Fatalf("unexpected client completion: mechanism=%d first=%x second=%x key=%x err=%v", mechanism, first, second, key, err)
			}
			if !bytes.Equal(s.private[:], make([]byte, 28)) || !bytes.Equal(s.passwordHash[:], make([]byte, 28)) {
				t.Fatal("ephemeral secret retained after exchange")
			}
			if _, _, err = s.finish(encodeSpakePacket(0, peer, proof)); err == nil {
				t.Fatal("accepted repeated exchange")
			}
		})
	}
}

func TestSpake224RejectsInvalidPeer(t *testing.T) {
	point, _ := hex.DecodeString(spake224N)
	for name, payload := range map[string][]byte{
		"wrong proof":  encodeSpakePacket(0, point, make([]byte, 32)),
		"short proof":  encodeSpakePacket(0, point, make([]byte, 31)),
		"bad point":    encodeSpakePacket(0, make([]byte, 56), make([]byte, 32)),
		"wrong curve":  encodeSpakePacket(1, point, make([]byte, 32)),
		"short header": make([]byte, 15),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := newSpake224("test", "password", "server", bytes.NewReader(bytes.Repeat([]byte{1}, 28)))
			if err != nil {
				t.Fatal(err)
			}
			if reply, key, err := s.finish(payload); err == nil || reply != nil || key != nil {
				t.Fatal("accepted invalid peer")
			}
		})
	}
}

func FuzzDecodeSpakePacket(f *testing.F) {
	f.Add(encodeSpakePacket(0, []byte("first"), []byte("second")))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		mechanism, first, second, err := decodeSpakePacket(payload)
		if err == nil && !bytes.Equal(encodeSpakePacket(mechanism, first, second), payload) {
			t.Fatal("SPAKE packet round trip mismatch")
		}
	})
}
