# Native transport and authentication

This document records the authentication portion of the native Exacq protocol,
independently of the SDK callback ABI. The implementation is in
`pkg/exacq/{crypto,spake,spakeplus,auth}.go`; framing and connection orchestration live in
`protocol.go` and `client.go`.

## Evidence and validation

The static oracle is Linux x64 `libevAPI.so` 1.47.27.1, distributed with
WebService 26.1.4.0. Its SHA-256 is
`14bd776c0fb0c46b72f2d2034a04ea2fa6a5794d0876facb1fc20cf4bda13509`.
Addresses below are ELF virtual addresses in this exact binary, before ASLR.

On 2026-10-21 the lab recorder advertised `NegotiateMechanisms="1"` and accepted
the pure Go transport, SPAKE mechanism 0, and authenticated configuration
request over the single forwarded native TCP port. The configuration response
was type 4/subtype 2 with 57,074 decoded bytes. This verifies the complete
mechanism-0 exchange, including the point constants, transcript hashes, CBC
envelope, and final receive-key transition. Recorder credentials, raw
configuration, and production authentication transcripts are not committed.

Earlier SDK traffic was independently decoded from raw TCP captures using the
transport DH shared secret. Those captures verified the discovery request,
discovery response, framing, compression and CBC layering. No assumptions
about C callback structures are used as wire evidence.

Unit evidence includes an independent OpenSSL CBC vector, a synthetic SPAKE
transcript calculated using separate Python affine P-224 arithmetic and
`hashlib`, and a reciprocal synthetic server that exercises authentication over
`net.Pipe`. The independent fixture uses invented credentials and recorder
identity. Mechanism 1 now has three independent fixtures produced by executing
the actual SDK's SPAKE2+ helpers with synthetic inputs. Its Go client and
multi-round transport state machine are tested against those outputs. On
2026-10-22 a fresh isolated Linux recorder 26.1.7.3 was migrated through the
supported SDK API: its advertisement changed from mask 1 to mask 3, and the
pure Go client completed mechanism-1 authentication and configuration retrieval.
Final qualification passed four correct logins, configuration/status/licence
reads, an incorrect-password rejection and a successful correct login afterward.
The rebuilt private inventory and health API also passed against this isolated
unlicensed recorder and the original trial recorder. See `spakeplus.md` and
`spakeplus-validation.json` for the evidence and qualification limits.

## Native framing and transforms

The 32-byte packet header is little-endian:

| Offset | Width | Field |
|---:|---:|---|
| 0 | 4 | Total packet length, including header |
| 4 | 4 | Packet type |
| 8 | 4 | Packet subtype |
| 12 | 2 | Status/result |
| 14 | 1 | Compression: 0 none, 1 zlib |
| 15 | 1 | Encryption: 0 none, 1 AES-CBC |
| 16 | 16 | Reserved bytes |
| 32 | remainder | Payload |

`production_connection::FQueueRequest` at `0x2599a0` constructs this header;
`production_connection::Read` at `0x25ae20` consumes it. The Go framing layer
also bounds encoded and decoded lengths and rejects unsupported transform
values. Media packets have their own inner structures; these are not derived
from the control framing.

Control payload transformation is zlib compression, then the following
AES-128-CBC envelope:

1. Pad the compressed data with zero bytes to a multiple of 16 bytes. An exact
   multiple receives no additional padding block.
2. Generate a 16-byte block R; prefix it to the padded data.
3. CBC-encrypt the entire prefix plus data with IV R.

Consequently the first ciphertext block is `AES_K(0^128)`. A decoder can discard
that block and decrypt the rest using that first ciphertext block as its IV.
Zero padding cannot be removed generically; the zlib stream marks the end of a
compressed payload. The SDK instead decrypts the whole buffer with its first
ciphertext block as IV and discards the resulting first plaintext block, which
has the same result. This is not PKCS#7, and the random prefix does not provide
randomized ciphertext because it cancels against the IV.

Evidence: `FRequestDone` at `0x25a850`, particularly padding and CBC setup at
`0x25a931..0x25a998`; receive CBC setup/decrypt at `0x25b3b0..0x25b415`.

## Initial transport exchange

Both session keys initially contain sixteen zero bytes
(`production_connection` constructor `0x259610`, initialization
`0x2598ef..0x259908`). The recorder speaks first:

1. Recorder sends type 7/subtype 1, carrying three little-endian uint64 values:
   generator g at offset 0, prime p at offset 8, server public B at offset 16.
2. Client chooses private a and computes `A = g^a mod p`, `S = B^a mod p`.
3. Client replies type 7/subtype 2 with 24 bytes: sixteen zero bytes followed by
   A as a little-endian uint64. This exchange packet is uncompressed and
   unencrypted.
4. The candidate 16-byte AES key is `LE64(S) || LE64(S)`.
5. After receiving the recorder's type 7/subtype 3 acknowledgement under the
   previous key, activate the new transport keys and send discovery.

Evidence: DH constructor arguments at `0x25b828`; repeated shared-secret stores
at `0x25bbcc..0x25bbfe`; reply construction at `0x25bc20..0x25bc86`.
The lab used 32-bit values inside these 64-bit fields. This finite-field
exchange provides transport compatibility, not authenticated recorder identity.
It is distinct from the password-authenticated SPAKE exchange below.

The SDK also supports repeating this transport exchange: type 7/subtype 3
copies the current transmit key to the receive key. It only initiates discovery
when the previous receive key is the initial zero key. Periodic rekey behaviour
has been statically traced but not exercised by this authentication validation.

The initial discovery request is type 1/subtype 1, transformed with zlib and
the negotiated AES key. The captured SDK body is:

```xml
<?xml version="1.0" encoding="UTF-8"?><eDVR Delta="1"><Client HeartbeatTimeout="30" MaxPacket="4194303" ConnectionSupports="159" ConnectionName="" ConnectionVersion="(1.47.27.1)"/></eDVR>
```

The Go client identifies its connection name as `go2rtc`. The response is type
1/subtype 2, containing `<eDVR><Servers><Server .../></Servers></eDVR>`. The lab
response begins with the invalid processing instruction `<?xml ?>`; the decoder
strips only this known prefix before strict XML decoding.

`CConnectionImpl::DoDiscover` is at `0x29d890`.
`GetStrongestNegotiationMethod` at `0x269fa0` interprets the server's
`NegotiateMechanisms` as a bitmask: bit 1 selects mechanism 1, otherwise bit 0
selects mechanism 0, otherwise legacy login. Go supports both advertised modes,
selects mechanism 1 when available, and returns an explicit unsupported-mechanism
error if neither is offered. It never downgrades after a failed exchange or
sends a plaintext-password login as an automatic fallback.

## SPAKE packet wrapper

All SPAKE messages are type 2/subtype 5. Their decoded payload contains:

| Offset | Width | Field |
|---:|---:|---|
| 0 | 1 | Mechanism number |
| 1 | 3 | Zero |
| 4 | 4 | First blob length, little-endian |
| 8 | 4 | Second blob length, little-endian |
| 12 | 4 | Zero |
| 16 | first length | First blob |
| 16 + first length | second length | Second blob |

Evidence: `production_connection::CreateSpakePacket` at `0x259a90`.
Go rejects inconsistent lengths and nonzero reserved wrapper fields.

## Mechanism 0: SPAKE2 on P-224

The curve is standard NIST P-224. Points are encoded as raw 28-byte big-endian
x followed by 28-byte big-endian y; there is no SEC1 prefix byte. Scalars are
big-endian. The fixed masking points are:

```text
M.x = 4d48c8ea8d23392e07e851fa6aa82048094e051372499c6fba62a74b
M.y = 6c185cabd52e2e8a9e2d21b0ec4ee141211fe29d64ea4d04463ae833
N.x = 0b1cfc6a407cdcb15dc1704cd13edaab8fdeff8cfbfb50d2c81de2c2
N.y = 3e14f62996080907b56dd282071aa7a121c39934bc30da5bcbc6a3cc
```

The binary stores these as Jacobian coordinates with z=1 and eight 28-bit limbs
per coordinate; M is at `0x6d3ca0` and N at `0x6d3c40`. Encoding and decoding
are established by `Point224::ToString` (`0x231110`), `SetFromString`
(`0x230fb0`), `Get224Bits` (`0x230520`), and the scalar-bit loop (`0x230f00`).

Let U be the UTF-8 username, P the raw UTF-8 password, and I the recorder's
`Serial` discovery attribute. No username normalization was observed.

```text
w = SHA-224(P)                              # 28-byte big-endian scalar
x = 28 random bytes                        # client private scalar
X = xG + wM
```

The client sends mechanism 0 with first blob `encode(X)` (56 bytes) and second
blob U. The server responds with first blob `encode(Y)` (56 bytes) and second
blob serverProof (32 bytes). Compute:

```text
S = encode(x * (Y - wN))                    # 56 bytes
```

The transcript hash is vendor-specific:

```text
H(fields...) = SHA-256(concat(decimalASCII(byteLength(field)), field))
```

There are no separators between lengths and data. An empty field contributes
the single ASCII byte `0`. This differs from binary length-prefix conventions.

```text
clientProof = H(U, encode(X), encode(Y), w, S)
serverProof = H(I, encode(X), encode(Y), w, S)        # I is nonempty
sessionHash = H(U, I, encode(X), encode(Y), w, S)
```

For older discovery documents with an empty I, the expected server proof is
instead `H(U, empty, encode(X), encode(Y), w, S)`. That fallback is statically
traced and covered by an independent synthetic fixture, but was not exercised
on the lab recorder.

The client validates the curve point, rejects the identity after unmasking, and
compares serverProof in constant time. Only after successful verification does
it send another type 2/subtype 5: first blob clientProof, second blob the
unchanged serverProof. **Send this packet under the existing transport key.**
Then replace the transmit key with `sessionHash[0:16]`.

The server's final type 2/subtype 2 packet with status 0 is encrypted using the
new key. Change the receive key before decrypting this packet. A failure status
does not trigger that receive-key change. Authentication is complete only
after the final acknowledgement; the Go login function stops there and leaves
configuration initialization to its caller. A malformed exchange, bad proof,
unsupported mechanism, nonzero final status or cancellation closes the
connection, without logging credential or proof material.

Evidence: `InitiateSpakeAuth` at `0x259be0`, password SHA-224 near `0x25a010`;
`Spake2Client<Point224>::Process` at `0x25f760`;
`PakePeer::CalculateHash` at `0x25f2a0`; final receive-key transition
`0x25b4f0..0x25b529`. `CConnectionImpl::Response` schedules subsequent audit,
license, configuration and status requests near `0x2ae2e7..0x2ae340`.

## Mechanism 1: SPAKE2+ on Edwards25519

Mechanism 1 is `Spake2PlusClient<Point25519>::Process` at `0x25fda0`. Static
tracing identifies a random clamped 32-byte scalar and an initial unmasked
curve point, a server-provided salt, and an Argon2i password derivation with
time cost 2, memory 16,384 KiB, four lanes and 64 output bytes
(`Salter` at `0x1f3500`). The two 32-byte halves are separately reduced modulo
the curve order. The later proof transcript includes two shared points.
The fixed masking point, salt echo, two shared points, proof transcripts and
session key are now verified against the executing SDK. The implementation
selects this mechanism when bit 1 is advertised and does not fall back after a
failed exchange. See `spakeplus.md` for exact messages, oracle addresses,
security checks and the distinction between SDK and recorder verification.

## Unsupported legacy authentication path

The legacy XML login document generated by `CConnectionImpl::GetLogin` at
`0x267ee0` is `<eDVR><Users><User Username="..." Password="..."
Type="evAPI"/></Users></eDVR>`. `SetLogin` at `0x27c6e0` stores the raw password.
This fallback is deliberately not implemented in the native client. The SDK's
separate `APIEncryptor` static key belongs to another configuration-secret
path, and is not the session AES key.

SDK internal object offsets used only to validate captures are:
`production_connection* = *(handle + 0x1e8)`, transmit `std::string` at
`production + 0x10138`, receive `std::string` at `production + 0x10158`.
Each string stores its data pointer followed by length. These are ABI-specific
oracle details, never wire layouts, and are not dependencies of the Go client.
