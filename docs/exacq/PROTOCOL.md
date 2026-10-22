# Exacq native protocol and integration

This specification describes the implemented and observed Exacq 26.1 native
TCP subset. It is sufficient for the existing ihub.js inventory, media,
historical snapshots, owner-scoped motion webhooks, explicit history search, health, and supported
speaker-output contracts. It is not a claim of every vendor API or recorder
generation. The production implementation is pure Go; no vendor shared library,
SDK gateway, CGO, RTSP restreamer, or second recorder port is required.

The evidence oracle was Linux x64 libevAPI 1.47.27.1 from WebService 26.1.4.0,
plus the symbol-bearing 26.1 client and actual recorder exchanges on 21 October
2026. Only allowlisted configuration fixtures and generated video are committed.
Credentials, camera footage, raw inventory and authentication captures remain
private. C callback layouts are never used as wire layouts.

## Specification parts

These documents together form the protocol specification. Static findings and
hardware observations are identified separately in each part.

| Part | Contents |
| --- | --- |
| [Authentication](auth-research.md) | Complete framing, CBC/zlib transform, initial DH, P-224 SPAKE constants/transcripts/proofs and key transitions |
| [SPAKE2+](spakeplus.md) | Mechanism 1 Edwards25519/Argon2i exchange, actual recorder negotiation, SDK fixtures and key transitions |
| [Media wire format](wire-research.md) | Packet enums, inner stream header, CASIDs, live/search XML, audio conversion and source evidence |
| [Control protocol](control-research.md) | Inventory/status schemas, owner-scoped motion updates, fresh readback, event-history XML and event-time JPEG selection |
| [Recorder webhooks](webhooks.md) | Native owner-scoped provisioning, recorder-originated push without an idle connection, and best-effort delivery |
| [License health](license.md) | Read-only binary key-presence response plus XML health classification; no license modification |
| [Producer](producer.md) | Source syntax, exact sample timing, finite playback, nearest keyframe, pacing, gaps, cancellation and limitations |
| [B-picture timing](b-pictures.md) | Fixed-count closed-GOP opt-in, recovered PTS, synthesized DTS, guards and recorder/SDK evidence |
| [SDK input comparison](sdk-input-equivalence.md) | Actual callback bytes and timestamp-multiset equivalence to native packets |
| [Platform contracts](platform-contracts.md) | Audited ihub capabilities and scope, channel mapping, durable cursor and owner rules |
| [Validation](validation.md) | Mixed hardware, soak, reconnect, snapshots, motion cleanup, tests and cross builds |

## Connection and message state

All integers below are little endian except the SPAKE point/scalar encodings.
A packet begins with 32 bytes: total length u32 at 0, type u32 at 4, subtype u32
at 8, status u16 at 12, compression u8 at 14, encryption u8 at 15, reserved16 at
16. Total length includes this header. Encoded and decoded payloads are bounded
at 32 MiB. Read the full declared record across arbitrary TCP segmentation.
Decrypt before decompressing. Compression 1 is zlib; encryption 1 is the
vendor AES-128-CBC envelope specified in the authentication part. Unsupported
transform values fail explicitly.

The recorder sends 7/1 DH; the client sends plain 7/2 and awaits 7/3. It sends
1/1 discovery under the derived key, reads 1/2, selects mechanism 1 when advertised
or mechanism 0 otherwise, performs SPAKE using 2/5 messages, validates the server
proof, then waits for successful 2/2 under
the new key. A nonzero status never authenticates. Request a full configuration
with empty 4/1 and receive 4/2. Delta configuration is not a complete inventory.
The connection sends an empty 1/1 heartbeat every ten seconds.

Periodic 7/1 rekey challenges are handled while reading other operations. The
transmit key changes atomically with the plain 7/2 reply, and the receive key
changes at 7/3. Unknown or overlapping rekeys fail. There is one reader per
session; independent media and control operations use separate sessions through
the same configured address/port. Advertised private recorder addresses are
never followed. A configured SOCKS route is used for every fresh session,
including motion verification; proxy failure never causes a direct dial.

| Operation | Request | Response |
| --- | --- | --- |
| Inventory | Empty 4/1 | Full 4/2 XML |
| Status | Empty 3/1 | 3/2 XML |
| Live video | 5/1 `Add/Stream@id,ref=1` | 5/3 media records |
| Recorded video | 6/1 `eDVR Type=1 Start End` plus camera hierarchy | 6/2 result XML, then Count media records in 6/3 |
| Motion history | 6/1 `Type=0` plus Events and source hierarchy | 6/2 result pages through `Events@Last=1` |
| Motion links | Narrow 4/3 delta XML | Broadcasts/acknowledgement; persistence established by a fresh login |
| License | Empty 9/1, then empty 3/1 | 9/2 key-presence data; 3/2 status XML |
| Speaker output | Paced 5/3 audio payloads | No physical-output acknowledgement |

Live/stored 5/3 and 6/3 payloads start with a separate 32-byte header: stream ID
u32 at 0, Unix seconds u32 at 4, milliseconds u16 at 8, flags u8 at 10, frame
kind u8 at 11, motion-byte length u32 at 12, reserved16 at 16. Motion bytes
follow, then elementary media. Frame kinds 1/2/3 mean I/P/B, independently of
inventory codec 5/6 for H.264/HEVC. A live request starts with the current GOP
so far, untimed (zero seconds); the producer back-dates it from the first timed
picture when picture order shows they are continuous (see producer.md). No separate decode timestamp has been
verified in this header or the SDK callback payload.

Complete recorder GOPs preserve the source presentation timestamp multiset,
but the timestamp attached to an individual decode-order packet is not reliably
its PTS. B-frame streams are therefore presented by codec POC at the measured
interval, re-anchored at each keyframe (`pkg/reorder`, shared with Milestone).

Recorded `Count` includes native preroll media. In addition, the lab recorder
sends a 6/3 boundary with exactly 32 bytes: `00 00 05 00` followed by 28 zeros.
It is excluded from Count and can arrive before the next search response. The
implementation skips only this exact record and never uses it as independent
proof of media EOF. This distinction prevents subsequent searches from failing
or truncating playback at a recording gap. Motion queries also observed it.

## Private control API

`POST /api/exacq`, `Content-Type: application/json`, uses the existing go2rtc API
binding and Basic authentication. Configure a private interface and API auth;
set `local_auth: true` when local clients must also authenticate. The body is
limited to 8 MiB, unknown fields/actions and URL query arguments are rejected,
and each operation has a 45-second outer timeout. Credentials are body fields,
never response fields. Errors and producer diagnostics redact credentials.

Common body fields: `action`, `address` (`host:port`), `username`, `password`,
optional `proxy` (`socks5[h]://user:password@host:port`). Response schemas use the
JSON names in the exported Go data types; the companion adapter maps them into
the audited ihub SDK-shaped inventory contract without invoking that SDK.

| Action | Additional body | Response |
| --- | --- | --- |
| `inventory` | None | Inventory with refreshed connection state |
| `health` | None | `{status, inventory, license}`; missing license evidence errors |
| `diagnose` | None | Always 200: `{ok, recorder?, steps[]}` stopping at the first failing step ([errors.md](./errors.md)) |
| `webhook.plan` | `ownerId`, `cameraIds`, `webhook:{url,authorization}` | Safe change counts, configured state and exact owned event-link/camera mapping |
| `webhook.apply` | Same; empty `cameraIds` removes owned configuration | `{configured,verified,links,changes}` after fresh-login readback |
| `playback.prepare` | `cameraId`, `startMs`, `endMs` | `{startsAtMs}`, shared nearest-keyframe rule |
| `snapshot` | `cameraId`, `width`, optional `atMs` | `{jpegBase64,capturedAtMs,width,height}`; in B-frame GOPs an I/P picture is chosen, so `capturedAtMs` is approximate by up to one mini-GOP |
| `talkdown` | `cameraId` (audio-output CASID), `frequency`, `pcmBase64` | `{accepted,acceptedBytes}` for sent PCM input bytes |

Wire-mode event delivery uses recorder-originated HTTP callbacks, not history
polling or a persistent native subscription. The destination must be reachable
from the recorder. Failed callbacks are best-effort: no retry queue, history
backfill or compensating connection is added. See [webhooks.md](webhooks.md).

History and playback intervals are positive, at most one hour and within the
verified signed-32-bit search epoch. A single video search is at most 60 seconds,
3,000 frames and 32 MiB; the producer uses ten-second windows. Snapshot conversion
uses local FFmpeg only for JPEG, with bounded buffers and cancellation. Historical
snapshots choose a picture at/after the event time within three seconds, retain
its preceding GOP and never substitute live footage. Talkdown is at most thirty
seconds of mono PCM16 at the output's exact rate, converted to native mu-law.

## Capability boundary

- Mechanism 0 on P-224 is verified against the recorder. SPAKE2+ / mechanism 1
  is verified against three actual SDK transcripts and an isolated actual
  recorder 26.1.7.3 after supported password strengthening. A failed authentication
  never downgrades. Legacy XML password login remains unsupported.
- Live/archive tracks support H.264 and H.265, including B-frames, without
  configuration. B-frame presentation times are reconstructed by POC and evenly
  spaced; only I/P streams keep the recorder's exact per-picture cadence.
- The producer exposes video. Incoming audio tracks, PTZ, generic vendor event
  targets, SMTP configuration, and camera provisioning are outside this
  adapter's implemented contract. Owner-scoped motion webhooks are supported. Discovery does not imply these capabilities.
  The current ihub Exacq HTTP core has no incoming-audio or PTZ method either;
  its generic FFmpeg audio-copy option does not establish a listen contract.
- Format-0 speaker talkdown supports the existing standalone-speaker contract.
  A bounded native chirp was detected by the device's internal microphone with
  a control comparison and exact settings restoration. This establishes signal
  reproduction for that test, not room coverage or speech intelligibility;
  send completion alone remains insufficient acoustic evidence.
- The ihub companion explicitly selects `wire` or `sdk` with SDK the existing
  default. Wire mode has no SDK/HTTP fallback. ihub.js supports the routing
  contract. The separately owned .NET host changes add private-API routing and
  URL globals; 104 focused tests pass, including the actual compiled wire
  transport in ClearScript. Those
  host changes are required for .NET use. Older runtimes fail before dialing.
- Ordinary parent/nested camera groups and audio-only exclusion passed on the
  real recorder through the compiled VMS adapter; see [groups.md](groups.md).
  Periodic rekeys retain synthetic/static coverage. No unlimited-session,
  long-term endurance, universal-version or post-2038 compatibility is claimed.

The legacy DH/CBC protocol itself does not provide modern authenticated TLS
transport. The implementation reproduces the verified native protocol; private
routing and the platform's established API authentication remain deployment
requirements. This work makes no production deployment or licensing changes.
