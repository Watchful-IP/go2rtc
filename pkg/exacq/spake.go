package exacq

import (
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
)

// Exacq's mechanism 0 uses NIST P-224 with these fixed masking points. They
// were recovered from libevAPI's Point224::{M,N} constants; the wire encoding
// is 28-byte big-endian X followed by 28-byte big-endian Y, without a prefix.
const (
	spake224M = "4d48c8ea8d23392e07e851fa6aa82048094e051372499c6fba62a74b6c185cabd52e2e8a9e2d21b0ec4ee141211fe29d64ea4d04463ae833"
	spake224N = "0b1cfc6a407cdcb15dc1704cd13edaab8fdeff8cfbfb50d2c81de2c23e14f62996080907b56dd282071aa7a121c39934bc30da5bcbc6a3cc"
)

type spake224 struct {
	username, server []byte
	passwordHash     [28]byte
	private          [28]byte
	public           []byte
	finished         bool
}

// errServerProof means the recorder could not prove knowledge of the
// password verifier: with SPAKE this is how a wrong password presents.
var errServerProof = errors.New("exacq: server authentication failed")

func newSpake224(username, password, server string, entropy io.Reader) (*spake224, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	s := &spake224{username: []byte(username), server: []byte(server), passwordHash: sha256.Sum224([]byte(password))}
	if _, err := io.ReadFull(entropy, s.private[:]); err != nil {
		return nil, fmt.Errorf("exacq: SPAKE entropy: %w", err)
	}
	curve := elliptic.P224()
	if new(big.Int).Mod(new(big.Int).SetBytes(s.private[:]), curve.Params().N).Sign() == 0 {
		return nil, errors.New("exacq: invalid SPAKE private scalar")
	}
	m, _ := hex.DecodeString(spake224M)
	mx, my, err := decodeP224(m)
	if err != nil {
		return nil, err
	}
	wx, wy := curve.ScalarMult(mx, my, s.passwordHash[:])
	x, y := curve.ScalarBaseMult(s.private[:])
	x, y = curve.Add(x, y, wx, wy)
	if x.Sign() == 0 && y.Sign() == 0 {
		return nil, errors.New("exacq: invalid SPAKE public point")
	}
	s.public = encodeP224(x, y)
	return s, nil
}

func (s *spake224) start() []byte {
	return encodeSpakePacket(0, s.public, s.username)
}

func (s *spake224) clear() {
	clear(s.private[:])
	clear(s.passwordHash[:])
}

// finish verifies the server's proof before producing the client's proof and
// the replacement AES-128 key. Send the returned packet under the previous
// transport key before installing the new transmit key.
func (s *spake224) finish(payload []byte) (reply, key []byte, err error) {
	if s.finished {
		return nil, nil, errors.New("exacq: SPAKE exchange already consumed")
	}
	s.finished = true
	defer clear(s.private[:])
	defer clear(s.passwordHash[:])
	mechanism, peer, proof, err := decodeSpakePacket(payload)
	if err != nil {
		return nil, nil, err
	}
	if mechanism != 0 || len(proof) != sha256.Size {
		return nil, nil, errors.New("exacq: invalid SPAKE response")
	}
	curve := elliptic.P224()
	px, py, err := decodeP224(peer)
	if err != nil {
		return nil, nil, err
	}
	n, _ := hex.DecodeString(spake224N)
	nx, ny, err := decodeP224(n)
	if err != nil {
		return nil, nil, err
	}
	wx, wy := curve.ScalarMult(nx, ny, s.passwordHash[:])
	wy.Sub(curve.Params().P, wy)
	x, y := curve.Add(px, py, wx, wy)
	if x.Sign() == 0 && y.Sign() == 0 {
		return nil, nil, errors.New("exacq: invalid SPAKE peer point")
	}
	x, y = curve.ScalarMult(x, y, s.private[:])
	shared := encodeP224(x, y)
	defer clear(shared)
	clientProof := pakeHash(s.username, s.public, peer, s.passwordHash[:], shared)
	var serverProof [32]byte
	if len(s.server) != 0 {
		serverProof = pakeHash(s.server, s.public, peer, s.passwordHash[:], shared)
	} else {
		// Older servers without a serial use the two-identity transcript,
		// including the ASCII zero length of the empty second identity.
		serverProof = pakeHash(s.username, nil, s.public, peer, s.passwordHash[:], shared)
	}
	if subtle.ConstantTimeCompare(proof, serverProof[:]) != 1 {
		return nil, nil, errServerProof
	}
	session := pakeHash(s.username, s.server, s.public, peer, s.passwordHash[:], shared)
	reply = encodeSpakePacket(0, clientProof[:], proof)
	key = append([]byte(nil), session[:16]...)
	clear(session[:])
	return reply, key, nil
}

// pakeHash uses the vendor's decimal ASCII lengths, not the binary length
// encoding used by other SPAKE implementations. Each length is followed
// immediately by the field's raw bytes, with no delimiter.
func pakeHash(fields ...[]byte) [32]byte {
	h := sha256.New()
	var length [20]byte
	for _, field := range fields {
		_, _ = h.Write(strconv.AppendInt(length[:0], int64(len(field)), 10))
		_, _ = h.Write(field)
	}
	var result [32]byte
	h.Sum(result[:0])
	return result
}

func encodeSpakePacket(mechanism byte, first, second []byte) []byte {
	payload := make([]byte, 16+len(first)+len(second))
	payload[0] = mechanism
	binary.LittleEndian.PutUint32(payload[4:], uint32(len(first)))
	binary.LittleEndian.PutUint32(payload[8:], uint32(len(second)))
	copy(payload[16:], first)
	copy(payload[16+len(first):], second)
	return payload
}

func decodeSpakePacket(payload []byte) (mechanism byte, first, second []byte, err error) {
	if len(payload) < 16 {
		return 0, nil, nil, errors.New("exacq: truncated SPAKE header")
	}
	n, m := uint64(binary.LittleEndian.Uint32(payload[4:])), uint64(binary.LittleEndian.Uint32(payload[8:]))
	if n+m+16 != uint64(len(payload)) {
		return 0, nil, nil, errors.New("exacq: invalid SPAKE blob lengths")
	}
	if payload[1] != 0 || payload[2] != 0 || payload[3] != 0 || binary.LittleEndian.Uint32(payload[12:]) != 0 {
		return 0, nil, nil, errors.New("exacq: unsupported SPAKE header")
	}
	return payload[0], payload[16 : 16+n], payload[16+n:], nil
}

func encodeP224(x, y *big.Int) []byte {
	point := make([]byte, 56)
	x.FillBytes(point[:28])
	y.FillBytes(point[28:])
	return point
}

func decodeP224(point []byte) (*big.Int, *big.Int, error) {
	if len(point) != 56 {
		return nil, nil, errors.New("exacq: invalid P224 point length")
	}
	x, y := new(big.Int).SetBytes(point[:28]), new(big.Int).SetBytes(point[28:])
	if !elliptic.P224().IsOnCurve(x, y) {
		return nil, nil, errors.New("exacq: invalid P224 point")
	}
	return x, y, nil
}
