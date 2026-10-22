package exacq

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/argon2"
)

// Mechanism 1 uses compressed Edwards25519 points, not Montgomery X25519.
// N is the encoded scalar-one result of the pinned SDK's fixed precomputation
// table. Its value and the complete transcript have SDK differential fixtures.
const spakePlusN = "10e3df0ae37d8e7a99b5fe74b44672103dbddcbd06af680d71329a11693bc778"

type spakePlus struct {
	username, server, password []byte
	private                    *edwards25519.Scalar
	public                     []byte
	serverProof, session       [32]byte
	state                      byte
	peerValid                  int
}

func newSpakePlus(username, password, server string, entropy io.Reader) (*spakePlus, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	var random [32]byte
	defer clear(random[:])
	if _, err := io.ReadFull(entropy, random[:]); err != nil {
		return nil, fmt.Errorf("exacq: SPAKE2+ entropy: %w", err)
	}
	private, _ := new(edwards25519.Scalar).SetBytesWithClamping(random[:])
	public := new(edwards25519.Point).ScalarBaseMult(private).Bytes()
	return &spakePlus{username: []byte(username), server: []byte(server), password: []byte(password), private: private, public: public, state: 1}, nil
}

func (s *spakePlus) start() []byte { return encodeSpakePacket(1, s.public, s.username) }

// finish first returns the client proof plus the echoed server salt under the
// old transport key. A subsequent server-proof message completes mutual
// authentication and returns a replacement key, with no further wire reply.
func (s *spakePlus) finish(payload []byte) (reply, key []byte, err error) {
	state := s.state
	s.state = 3 // Consume even malformed input; never retry a failed exchange.
	defer func() {
		if s.state == 3 {
			s.clear()
		}
	}()
	mechanism, first, second, err := decodeSpakePacket(payload)
	if err != nil {
		return nil, nil, err
	}
	if mechanism != 1 {
		return nil, nil, errors.New("exacq: unexpected SPAKE2+ mechanism")
	}
	switch state {
	case 1:
		if len(first) != 32 || len(second) < 8 || len(second) > 1024 {
			return nil, nil, errors.New("exacq: invalid SPAKE2+ challenge")
		}
		peer, err := decodeEdwardsPoint(first)
		if err != nil {
			return nil, nil, err
		}
		// Always generate the dummy, including for valid peers. A rejection
		// immediately after unmasking would reveal w0 modulo eight because N
		// has an order-eight component. Invalid points receive an opaque proof
		// and are rejected only in the server-proof phase, without a key.
		var dummyProof [32]byte
		defer clear(dummyProof[:])
		if _, err = io.ReadFull(rand.Reader, dummyProof[:]); err != nil {
			return nil, nil, errors.New("exacq: SPAKE2+ proof entropy failed")
		}
		// Parameters are fixed by Salter in libevAPI: Argon2i v1.3, two
		// iterations, 16384 KiB, four lanes, 64 output bytes. The two halves
		// are independently reduced from 256-bit little-endian integers.
		derived := argon2.Key(s.password, second, 2, 16384, 4, 64)
		defer clear(derived)
		clear(s.password)
		w0, w1 := reduceSpakeScalar(derived[:32]), reduceSpakeScalar(derived[32:])
		defer w0.Set(edwards25519.NewScalar())
		defer w1.Set(edwards25519.NewScalar())
		nBytes, _ := hex.DecodeString(spakePlusN)
		n, _ := new(edwards25519.Point).SetBytes(nBytes)
		unmasked := new(edwards25519.Point).Subtract(peer, new(edwards25519.Point).ScalarMult(w0, n))
		sharedPoint, valid := prepareSpakePeer(unmasked)
		s.peerValid = valid
		z := new(edwards25519.Point).ScalarMult(s.private, sharedPoint).Bytes()
		v := new(edwards25519.Point).ScalarMult(w1, sharedPoint).Bytes()
		defer clear(z)
		defer clear(v)
		s.private.Set(edwards25519.NewScalar())
		clientProof := pakeHash(s.username, s.public, first, z, v)
		defer clear(clientProof[:])
		subtle.ConstantTimeCopy(1-valid, clientProof[:], dummyProof[:])
		s.serverProof = pakeHash(s.server, s.public, first, z, v)
		s.session = pakeHash(s.username, s.server, s.public, first, z, v)
		s.state = 2
		return encodeSpakePacket(1, clientProof[:], second), nil, nil
	case 2:
		// The SDK ignores blob2 in this phase. Bound it rather than treating
		// it as authenticated data or changing the proven proof transcript.
		if len(first) != 32 || len(second) > 1024 || subtle.ConstantTimeCompare(first, s.serverProof[:])&s.peerValid != 1 {
			return nil, nil, errServerProof
		}
		return nil, append([]byte(nil), s.session[:16]...), nil
	default:
		return nil, nil, errors.New("exacq: SPAKE2+ exchange already consumed")
	}
}

func (s *spakePlus) clear() {
	clear(s.password)
	clear(s.serverProof[:])
	clear(s.session[:])
	s.peerValid = 0
	if s.private != nil {
		s.private.Set(edwards25519.NewScalar())
	}
}

func reduceSpakeScalar(data []byte) *edwards25519.Scalar {
	var wide [64]byte
	defer clear(wide[:])
	copy(wide[:32], data)
	s, _ := new(edwards25519.Scalar).SetUniformBytes(wide[:])
	return s
}

func decodeEdwardsPoint(data []byte) (*edwards25519.Point, error) {
	p, err := new(edwards25519.Point).SetBytes(data)
	if err != nil || !bytes.Equal(p.Bytes(), data) {
		return nil, errors.New("exacq: invalid Edwards25519 point")
	}
	return p, nil
}

// prepareSpakePeer returns a nonidentity prime-order point for shared-point
// computation, and separately records whether the input met that requirement.
// All inputs perform the same projection and selection work. Invalid inputs
// must receive a random proof, never a real proof computed from the substitute
// generator: for that point Z=X and V=w1*G would be an offline password verifier.
// The caller defers rejection until server-proof processing to avoid revealing
// password-derived information through the mask's order-eight component.
func prepareSpakePeer(p *edwards25519.Point) (*edwards25519.Point, int) {
	eight, _ := new(edwards25519.Scalar).SetCanonicalBytes(append([]byte{8}, make([]byte, 31)...))
	inverse := new(edwards25519.Scalar).Invert(eight)
	projected := new(edwards25519.Point).MultByCofactor(p)
	nonidentity := 1 - projected.Equal(edwards25519.NewIdentityPoint())
	projected.ScalarMult(inverse, projected)
	valid := nonidentity & projected.Equal(p)
	return new(edwards25519.Point).Select(projected, edwards25519.NewGeneratorPoint(), nonidentity), valid
}
