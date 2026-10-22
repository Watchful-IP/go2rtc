package exacq

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"testing"
)

func TestPayloadCBCOracle(t *testing.T) {
	// Independently generated using openssl enc -aes-128-cbc -nopad with
	// zero key, IV 000102...0f, plaintext IV || "<eDVR/>" || nine zeros.
	want, _ := hex.DecodeString("66e94bd4ef8a2c3b884cfa59ca342b2e280c98f4ac30a387a8c52d5d4151094b")
	entropy := make([]byte, 16)
	for i := range entropy {
		entropy[i] = byte(i)
	}
	got, err := encryptPayload([]byte("<eDVR/>"), make([]byte, 16), bytes.NewReader(entropy))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ciphertext = %x, err = %v", got, err)
	}
	clear, err := decryptPayload(want, make([]byte, 16))
	if err != nil || !bytes.Equal(clear, append([]byte("<eDVR/>"), make([]byte, 9)...)) {
		t.Fatalf("plaintext = %x, err = %v", clear, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("decrypt modified its input")
	}
}

func TestPayloadCBCRoundTripBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 65536} {
		plain := bytes.Repeat([]byte{0x79}, n)
		key := bytes.Repeat([]byte{0x42}, 16)
		sealed, err := encryptPayload(plain, key, bytes.NewReader(make([]byte, 16)))
		if err != nil {
			t.Fatal(err)
		}
		clear, err := decryptPayload(sealed, key)
		if err != nil || len(clear) != (n+15)/16*16 || !bytes.Equal(clear[:n], plain) {
			t.Fatalf("size %d: bad plaintext or padding, err = %v", n, err)
		}
		if !bytes.Equal(clear[n:], make([]byte, len(clear)-n)) {
			t.Fatalf("size %d: nonzero padding", n)
		}
	}
}

func TestPayloadCBCMalformed(t *testing.T) {
	for _, n := range []int{0, 1, 15, 17, 31, 33} {
		if _, err := decryptPayload(make([]byte, n), make([]byte, 16)); err == nil {
			t.Fatalf("accepted ciphertext length %d", n)
		}
	}
	if _, err := decryptPayload(make([]byte, 16), make([]byte, 15)); err == nil {
		t.Fatal("accepted invalid AES key")
	}
	if _, err := encryptPayload([]byte("x"), make([]byte, 16), bytes.NewReader(nil)); err == nil {
		t.Fatal("ignored entropy failure")
	}
}

func TestLegacyDHResponse(t *testing.T) {
	// Classic DH example: p=23, g=5, a=6, B=19 -> A=8, shared=2.
	challenge := make([]byte, 24)
	binary.LittleEndian.PutUint64(challenge, 5)
	binary.LittleEndian.PutUint64(challenge[8:], 23)
	binary.LittleEndian.PutUint64(challenge[16:], 19)
	response, key, err := legacyDHResponse(challenge, bytes.NewReader([]byte{4}))
	if err != nil {
		t.Fatal(err)
	}
	if len(response) != 24 || !bytes.Equal(response[:16], make([]byte, 16)) || binary.LittleEndian.Uint64(response[16:]) != 8 {
		t.Fatalf("response = %x", response)
	}
	if len(key) != 16 || binary.LittleEndian.Uint64(key) != 2 || !bytes.Equal(key[:8], key[8:]) {
		t.Fatalf("key = %x", key)
	}
}

func TestLegacyDHChallengeBounds(t *testing.T) {
	for _, values := range [][3]uint64{{0, 23, 19}, {1, 23, 19}, {23, 23, 19}, {5, 0, 19}, {5, 21, 19}, {5, 23, 0}, {5, 23, 1}, {5, 23, 22}, {5, 23, 23}} {
		challenge := make([]byte, 24)
		for i, v := range values {
			binary.LittleEndian.PutUint64(challenge[i*8:], v)
		}
		if _, _, err := legacyDHResponse(challenge, bytes.NewReader([]byte{4})); err == nil {
			t.Fatalf("accepted DH parameters %v", values)
		}
	}
	if _, _, err := legacyDHResponse(make([]byte, 23), nil); err == nil {
		t.Fatal("accepted truncated challenge")
	}
}

func FuzzDecryptPayload(f *testing.F) {
	f.Add(make([]byte, 32), make([]byte, 16))
	f.Add([]byte{1, 2, 3}, []byte{4})
	f.Fuzz(func(t *testing.T, payload, key []byte) {
		_, _ = decryptPayload(payload, key)
	})
}

func FuzzLegacyDHResponse(f *testing.F) {
	f.Add(make([]byte, 24))
	f.Fuzz(func(t *testing.T, challenge []byte) {
		// Bounded entropy prevents a malformed modulus from driving a
		// pathological rejection-sampling loop in this adversarial test.
		_, _, _ = legacyDHResponse(challenge, io.LimitReader(bytes.NewReader(make([]byte, 64)), 64))
	})
}
