package exacq

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"filippo.io/edwards25519"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
)

type plusFixture struct {
	Username, Server, Salt, Private, Public, Peer, Argon2, W0, W1, Z, V, Session string
	PasswordHex                                                                  string `json:"password_hex"`
	MaskN                                                                        string `json:"mask_n"`
	ClientProof                                                                  string `json:"client_proof"`
	ServerProof                                                                  string `json:"server_proof"`
}

func spakePlusFixtures(t *testing.T) []plusFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/spakeplus.json")
	require.NoError(t, err)
	var fixtures struct{ Fixtures []plusFixture }
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.Len(t, fixtures.Fixtures, 3)
	return fixtures.Fixtures
}

func unhex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// Fixtures were produced by calling the real SDK's Process, SetBlobs, Salter,
// scalar reduction and point helpers in an isolated Linux process. They are
// synthetic test credentials, never recorder captures or real account data.
func TestSpakePlusSDKFixtures(t *testing.T) {
	for i, fixture := range spakePlusFixtures(t) {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s, err := newSpakePlus(fixture.Username, string(unhex(fixture.PasswordHex)), fixture.Server, bytes.NewReader(unhex(fixture.Private)))
			require.NoError(t, err)
			defer s.clear()
			mechanism, public, username, err := decodeSpakePacket(s.start())
			require.NoError(t, err)
			require.EqualValues(t, 1, mechanism)
			require.Equal(t, unhex(fixture.Public), public)
			require.Equal(t, fixture.Username, string(username))
			require.Equal(t, spakePlusN, fixture.MaskN)
			derived := argon2.Key(unhex(fixture.PasswordHex), unhex(fixture.Salt), 2, 16384, 4, 64)
			require.Equal(t, unhex(fixture.Argon2), derived)
			require.Equal(t, unhex(fixture.W0), reduceSpakeScalar(derived[:32]).Bytes())
			require.Equal(t, unhex(fixture.W1), reduceSpakeScalar(derived[32:]).Bytes())
			reply, key, err := s.finish(encodeSpakePacket(1, unhex(fixture.Peer), unhex(fixture.Salt)))
			require.NoError(t, err)
			require.Nil(t, key, "server authentication must precede key release")
			mechanism, proof, salt, err := decodeSpakePacket(reply)
			require.NoError(t, err)
			require.EqualValues(t, 1, mechanism)
			require.Equal(t, unhex(fixture.ClientProof), proof)
			require.Equal(t, unhex(fixture.Salt), salt, "SDK echoes salt in second blob")
			require.Equal(t, unhex(fixture.ServerProof), s.serverProof[:])
			require.Equal(t, unhex(fixture.Session), s.session[:])
			require.Equal(t, edwards25519.NewScalar().Bytes(), s.private.Bytes())
			require.Equal(t, make([]byte, len(s.password)), s.password)
			reply, key, err = s.finish(encodeSpakePacket(1, unhex(fixture.ServerProof), nil))
			require.NoError(t, err)
			require.Nil(t, reply, "SDK sends no acknowledgement after server proof")
			require.Equal(t, unhex(fixture.Session)[:16], key)
			require.Equal(t, [32]byte{}, s.session)
			require.Equal(t, [32]byte{}, s.serverProof)
			_, _, err = s.finish(encodeSpakePacket(1, unhex(fixture.ServerProof), nil))
			require.Error(t, err)
		})
	}
}

func TestSpakePlusRejectsInvalidInput(t *testing.T) {
	fixture := spakePlusFixtures(t)[0]
	peer, salt := unhex(fixture.Peer), unhex(fixture.Salt)
	for name, challenge := range map[string][]byte{
		"short header":    make([]byte, 15),
		"wrong mechanism": encodeSpakePacket(0, peer, salt),
		"short point":     encodeSpakePacket(1, peer[:31], salt),
		"invalid point":   encodeSpakePacket(1, bytes.Repeat([]byte{0xff}, 32), salt),
		"short salt":      encodeSpakePacket(1, peer, salt[:7]),
		"long salt":       encodeSpakePacket(1, peer, make([]byte, 1025)),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := newSpakePlus(fixture.Username, string(unhex(fixture.PasswordHex)), fixture.Server, bytes.NewReader(unhex(fixture.Private)))
			require.NoError(t, err)
			reply, key, err := s.finish(challenge)
			require.Error(t, err)
			require.Nil(t, reply)
			require.Nil(t, key)
			_, _, err = s.finish(encodeSpakePacket(1, peer, salt))
			require.Error(t, err, "malformed exchange cannot be retried")
		})
	}
	for _, proof := range [][]byte{nil, make([]byte, 32), bytes.Repeat([]byte{1}, 33)} {
		s, err := newSpakePlus(fixture.Username, string(unhex(fixture.PasswordHex)), fixture.Server, bytes.NewReader(unhex(fixture.Private)))
		require.NoError(t, err)
		_, _, err = s.finish(encodeSpakePacket(1, peer, salt))
		require.NoError(t, err)
		reply, key, err := s.finish(encodeSpakePacket(1, proof, nil))
		require.Error(t, err)
		require.Nil(t, reply)
		require.Nil(t, key)
		require.Equal(t, [32]byte{}, s.session)
	}
	_, err := newSpakePlus("u", "p", "s", bytes.NewReader(nil))
	require.Error(t, err)
}

func TestSpakePlusRejectsTorsionAndIdentity(t *testing.T) {
	identity := edwards25519.NewIdentityPoint()
	// Canonical order-two point (0,-1).
	torsion, err := decodeEdwardsPoint(unhex("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"))
	require.NoError(t, err)
	for _, p := range []*edwards25519.Point{identity, torsion, new(edwards25519.Point).Add(edwards25519.NewGeneratorPoint(), torsion)} {
		safe, valid := prepareSpakePeer(p)
		require.Zero(t, valid)
		require.NotEqual(t, identity.Bytes(), safe.Bytes())
		_, safeValid := prepareSpakePeer(safe)
		require.Equal(t, 1, safeValid)
	}
	_, valid := prepareSpakePeer(edwards25519.NewGeneratorPoint())
	require.Equal(t, 1, valid)
	fixture := spakePlusFixtures(t)[0]
	w0, _ := new(edwards25519.Scalar).SetCanonicalBytes(unhex(fixture.W0))
	w1, _ := new(edwards25519.Scalar).SetCanonicalBytes(unhex(fixture.W1))
	verifier := new(edwards25519.Point).ScalarBaseMult(w1).Bytes()
	n, err := decodeEdwardsPoint(unhex(spakePlusN))
	require.NoError(t, err)
	mask := new(edwards25519.Point).ScalarMult(w0, n)
	for _, unmasked := range []*edwards25519.Point{identity, torsion, new(edwards25519.Point).Add(edwards25519.NewGeneratorPoint(), torsion)} {
		s, err := newSpakePlus(fixture.Username, string(unhex(fixture.PasswordHex)), fixture.Server, bytes.NewReader(unhex(fixture.Private)))
		require.NoError(t, err)
		peer := new(edwards25519.Point).Add(mask, unmasked).Bytes()
		reply, key, err := s.finish(encodeSpakePacket(1, peer, unhex(fixture.Salt)))
		require.NoError(t, err, "do not reveal the password-dependent invalid point in this phase")
		require.Len(t, reply, 16+32+len(unhex(fixture.Salt)))
		require.Nil(t, key)
		_, proof, _, err := decodeSpakePacket(reply)
		require.NoError(t, err)
		// All three substituted/projected points here are G. Publishing the
		// real proof for G would let an observer test passwords offline using
		// the public X and a guessed w1*G. The dummy must replace that proof.
		unsafeProof := pakeHash([]byte(fixture.Username), s.public, peer, s.public, verifier)
		require.NotEqual(t, unsafeProof[:], proof)
		serverProof := append([]byte(nil), s.serverProof[:]...)
		reply, key, err = s.finish(encodeSpakePacket(1, serverProof, nil))
		require.EqualError(t, err, "exacq: server authentication failed")
		require.Nil(t, reply)
		require.Nil(t, key)
		require.Zero(t, s.peerValid)
		require.Equal(t, [32]byte{}, s.session)
		_, _, err = s.finish(encodeSpakePacket(1, serverProof, nil))
		require.Error(t, err, "failed exchange is consumed")
	}
}

// N has a nonzero order-eight component. A server can choose the torsion
// component of Y without knowing the password. Rejecting Y-w0*N before sending
// the client proof would reveal one of eight password-derived scalar classes.
func TestSpakePlusChallengeDoesNotRevealPasswordPartition(t *testing.T) {
	fixture := spakePlusFixtures(t)[0]
	n, err := decodeEdwardsPoint(unhex(spakePlusN))
	require.NoError(t, err)
	eight, _ := new(edwards25519.Scalar).SetCanonicalBytes(append([]byte{8}, make([]byte, 31)...))
	projected := new(edwards25519.Point).MultByCofactor(n)
	projected.ScalarMult(new(edwards25519.Scalar).Invert(eight), projected)
	torsion := new(edwards25519.Point).Subtract(n, projected)
	require.NotEqual(t, edwards25519.NewIdentityPoint().Bytes(), torsion.Bytes())
	require.Equal(t, edwards25519.NewIdentityPoint().Bytes(), new(edwards25519.Point).MultByCofactor(torsion).Bytes())
	w0 := unhex(fixture.W0)
	validClass := int(w0[0] & 7)
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			encoded := make([]byte, 32)
			encoded[0] = byte(i)
			scalar, err := new(edwards25519.Scalar).SetCanonicalBytes(encoded)
			require.NoError(t, err)
			peer := new(edwards25519.Point).Add(edwards25519.NewGeneratorPoint(), new(edwards25519.Point).ScalarMult(scalar, torsion))
			s, err := newSpakePlus(fixture.Username, string(unhex(fixture.PasswordHex)), fixture.Server, bytes.NewReader(unhex(fixture.Private)))
			require.NoError(t, err)
			defer s.clear()
			reply, key, err := s.finish(encodeSpakePacket(1, peer.Bytes(), unhex(fixture.Salt)))
			require.NoError(t, err, "all torsion classes must reach the same proof phase")
			require.Nil(t, key)
			mechanism, proof, salt, err := decodeSpakePacket(reply)
			require.NoError(t, err)
			require.EqualValues(t, 1, mechanism)
			require.Len(t, proof, 32)
			require.Equal(t, unhex(fixture.Salt), salt)
			// Even knowledge of the internal expected proof must not release a
			// key for an invalid point. Real peers cannot see this value.
			serverProof := append([]byte(nil), s.serverProof[:]...)
			reply, key, err = s.finish(encodeSpakePacket(1, serverProof, nil))
			require.Nil(t, reply)
			if i == validClass {
				require.NoError(t, err)
				require.Len(t, key, 16)
			} else {
				require.EqualError(t, err, "exacq: server authentication failed")
				require.Nil(t, key)
			}
			require.Equal(t, [32]byte{}, s.session)
			require.Equal(t, [32]byte{}, s.serverProof)
		})
	}
}

func TestLoginSpakePlusKeyTransition(t *testing.T) {
	for _, mode := range []string{"valid", "badProof", "invalidPeer", "prematureAck", "cancel", "cancelChallenge"} {
		t.Run(mode, func(t *testing.T) {
			client, server := sessionPair(t)
			client.options.Username, client.options.Password = "plus-user", "plus-password"
			client.discovery = Discovery{Serial: "PLUS-RECORDER", NegotiateMechanisms: 3}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- respondSpakePlus(server, mode, cancel) }()
			err := client.login(ctx)
			if mode == "valid" {
				require.NoError(t, err)
				require.False(t, client.loginKeyPending)
				require.Equal(t, client.txKey, client.rxKey)
				require.NotEqual(t, make([]byte, 16), client.txKey)
			} else {
				require.Error(t, err)
				require.Equal(t, make([]byte, 16), client.txKey, "no rekey before valid server proof")
			}
			require.NoError(t, <-done)
		})
	}
}

func respondSpakePlus(server *Client, mode string, cancel context.CancelFunc) error {
	p, err := server.readPacket()
	if err != nil {
		return err
	}
	mechanism, public, username, err := decodeSpakePacket(p.Payload)
	if err != nil || p.Type != packetLogin || p.Subtype != 5 || mechanism != 1 || string(username) != "plus-user" {
		return errors.New("incorrect SPAKE2+ initial request or mechanism preference")
	}
	if mode == "cancel" {
		cancel()
		return nil
	}
	salt := []byte("mock-server-salt")
	derived := argon2.Key([]byte("plus-password"), salt, 2, 16384, 4, 64)
	w0, w1 := reduceSpakeScalar(derived[:32]), reduceSpakeScalar(derived[32:])
	y, _ := new(edwards25519.Scalar).SetBytesWithClamping(bytes.Repeat([]byte{0x61}, 32))
	n, _ := decodeEdwardsPoint(unhex(spakePlusN))
	yG := new(edwards25519.Point).ScalarBaseMult(y)
	peer := new(edwards25519.Point).Add(yG, new(edwards25519.Point).ScalarMult(w0, n)).Bytes()
	if mode == "invalidPeer" {
		peer = new(edwards25519.Point).ScalarMult(w0, n).Bytes() // Q is identity.
	}
	x, err := decodeEdwardsPoint(public)
	if err != nil {
		return err
	}
	z := new(edwards25519.Point).ScalarMult(y, x).Bytes()
	v := new(edwards25519.Point).ScalarMult(w1, yG).Bytes()
	if err = server.sendPacket(packetLogin, 5, encodeSpakePacket(1, peer, salt)); err != nil {
		return err
	}
	if mode == "cancelChallenge" {
		cancel()
		return nil
	}
	p, err = server.readPacket() // Still decrypts under old key.
	if err != nil {
		return err
	}
	mechanism, proof, echo, err := decodeSpakePacket(p.Payload)
	want := pakeHash(username, public, peer, z, v)
	if err != nil || mechanism != 1 || len(proof) != 32 || !bytes.Equal(echo, salt) || (mode != "invalidPeer" && !bytes.Equal(proof, want[:])) {
		return errors.New("incorrect SPAKE2+ proof or echoed salt")
	}
	if mode == "prematureAck" {
		return server.sendPacket(packetLogin, 2, []byte(`<eDVR/>`))
	}
	serverProof := pakeHash([]byte("PLUS-RECORDER"), public, peer, z, v)
	if mode == "badProof" || mode == "invalidPeer" {
		serverProof[0] ^= 1
	}
	if err = server.sendPacket(packetLogin, 5, encodeSpakePacket(1, serverProof[:], nil)); err != nil {
		return err
	}
	if mode == "badProof" || mode == "invalidPeer" {
		_ = server.conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err = server.readPacket(); err == nil {
			return errors.New("client continued after bad server proof")
		}
		return nil
	}
	key := pakeHash(username, []byte("PLUS-RECORDER"), public, peer, z, v)
	server.txKey = key[:16]
	return server.sendPacket(packetLogin, 2, []byte(`<eDVR/>`))
}
