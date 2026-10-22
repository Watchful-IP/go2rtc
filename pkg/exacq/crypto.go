package exacq

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
)

// encryptPayload implements the native transport's CBC envelope. The first
// plaintext block is random and is also the IV; it is encrypted along with the
// payload. Thus its ciphertext is AES(key, zero), and subsequent blocks use
// ordinary CBC chaining. Payload padding consists of zero bytes, not PKCS#7.
// Compression, when requested, must happen before this function is called.
func encryptPayload(plain, key []byte, entropy io.Reader) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(plain) > int(^uint(0)>>1)-2*aes.BlockSize {
		return nil, errors.New("exacq: payload too large")
	}
	if entropy == nil {
		entropy = rand.Reader
	}
	n := (len(plain) + aes.BlockSize - 1) &^ (aes.BlockSize - 1)
	out := make([]byte, aes.BlockSize+n)
	if _, err = io.ReadFull(entropy, out[:aes.BlockSize]); err != nil {
		return nil, fmt.Errorf("exacq: payload entropy: %w", err)
	}
	copy(out[aes.BlockSize:], plain)
	// NewCBCEncrypter copies the IV before in-place encryption overwrites it.
	cipher.NewCBCEncrypter(block, out[:aes.BlockSize]).CryptBlocks(out, out)
	return out, nil
}

// decryptPayload removes the CBC envelope but retains its zero padding: there
// is no plaintext length or padding count in this layer. For compressed control
// messages, the zlib stream itself provides the end of the payload.
func decryptPayload(payload, key []byte) ([]byte, error) {
	if len(payload) < aes.BlockSize || len(payload)%aes.BlockSize != 0 {
		return nil, errors.New("exacq: invalid encrypted payload length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(payload)-aes.BlockSize)
	// The SDK decrypts the prefix too, then discards it. Skipping that block
	// and using its ciphertext as the IV is equivalent and avoids the garbage
	// plaintext that the SDK produces for that deliberately ignored block.
	cipher.NewCBCDecrypter(block, payload[:aes.BlockSize]).CryptBlocks(out, payload[aes.BlockSize:])
	return out, nil
}

// legacyDHResponse handles the native type 7 / subtype 1 transport challenge.
// All three 64-bit integers are little-endian on the wire. This is the vendor's
// legacy transport exchange, not the password-authenticated SPAKE exchange.
// The response is type 7 / subtype 2; the caller owns packet framing and key
// activation. The negotiated key must not become the receive key before the
// server's type 7 / subtype 3 acknowledgement has been processed.
func legacyDHResponse(challenge []byte, entropy io.Reader) (response, key []byte, err error) {
	if len(challenge) != 24 {
		return nil, nil, errors.New("exacq: invalid DH challenge length")
	}
	g := new(big.Int).SetUint64(binary.LittleEndian.Uint64(challenge))
	p := new(big.Int).SetUint64(binary.LittleEndian.Uint64(challenge[8:]))
	peer := new(big.Int).SetUint64(binary.LittleEndian.Uint64(challenge[16:]))
	one := big.NewInt(1)
	upper := new(big.Int).Sub(p, one)
	if p.Cmp(big.NewInt(5)) < 0 || !p.ProbablyPrime(32) ||
		g.Cmp(one) <= 0 || g.Cmp(upper) >= 0 ||
		peer.Cmp(one) <= 0 || peer.Cmp(upper) >= 0 {
		return nil, nil, errors.New("exacq: invalid DH parameters")
	}
	if entropy == nil {
		entropy = rand.Reader
	}
	// Choose 2 <= secret <= p-2. This finite-field exchange is retained for
	// protocol compatibility; it does not establish the server's identity.
	limit := new(big.Int).Sub(p, big.NewInt(3))
	secret, err := rand.Int(entropy, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("exacq: DH entropy: %w", err)
	}
	secret.Add(secret, big.NewInt(2))
	public := new(big.Int).Exp(g, secret, p)
	shared := new(big.Int).Exp(peer, secret, p)
	if public.Cmp(one) <= 0 || public.Cmp(upper) >= 0 || shared.Cmp(one) <= 0 {
		return nil, nil, errors.New("exacq: degenerate DH exchange")
	}
	response = make([]byte, 24)
	binary.LittleEndian.PutUint64(response[16:], public.Uint64())
	key = make([]byte, aes.BlockSize)
	binary.LittleEndian.PutUint64(key, shared.Uint64())
	copy(key[8:], key[:8])
	return response, key, nil
}
