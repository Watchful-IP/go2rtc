# Exacq platform contracts and implementation boundaries

This document records the existing Watchful/ihub contract inspected on 21 October
2026 and recommends the native go2rtc interface needed to support it. It is a
requirements/source audit, not evidence that a pure Go wire operation works.
The SDK adapter is a behavioural oracle; its C structures, callback numbers and
memory offsets are not wire-format specifications.

## Source map

References prefixed `ihub/` are relative to the Watchful-IP/ihub repository at
the time of this research; they can change independently.
Line numbers below identify the inspected revision; function names are the stable
reference. Unprefixed references are relative to this go2rtc worktree.

| Area | Exact reference |
| --- | --- |
| Native public data/API contract | `ihub/integrations/exacqvision-http-shared/native-sdk.ts:2`, `ExacqOpenParams`, `ExacqNativeSdk` at line 116 |
| Native platform adapter | `ihub/integrations/exacqvision-http-shared/native-client.ts:35`, `ExacqNativeClient` |
| Shared NVR/VMS behaviour | `ihub/integrations/exacqvision-http-shared/core.ts:100`, `ExacqVisionHttpCore` |
| Persisted discovery and delivery | `ihub/integrations/exacqvision-http-shared/native-events.ts:83`, `pollNativeEvents`; line 152, `deliverNativeEvent` |
| Existing SDK session behaviour | `ihub/servers/exacq-sdk/src/session.ts:66`, `ExacqSession.open`; line 366, `close` |
| Existing SDK media behaviour | `ihub/servers/exacq-sdk/src/archive.ts:40`, `captureSnapshot`; line 197, `startPlayback`; `src/live-video.ts:5`, `videoSink` |
| Owner-scoped motion configuration | `ihub/servers/exacq-sdk/src/motion-logging.ts:25`, `motionLogGuid`; line 56, `configureMotionLogging` |
| Optional webhook configuration | `ihub/servers/exacq-sdk/src/alerting.ts:37`, `configureAlerting`; `src/native-alerting.ts:43`, `loadAlerting` |
| Existing validation claims and limits | `ihub/servers/exacq-sdk/README.txt`, sections “Media and sessions”, “Native event configuration”, “Hardware validation” |

## Required connection and inventory contract

`NativeHost` opts into native mode; `NativePort` defaults to 22609. Recorder
`Username` and `Password` are required. `URL` is unnecessary in native mode and
there is no silent Web Service fallback (`core.ts`, `#applyVariables`;
`native-client.ts`, constructor). Use one configured destination host/port for
control, media and events. This permits multiple TCP sessions through one forward;
it does not imply all operations share one socket or unlimited recorder sessions.

Native sessions may share an account. The HTTP mode's derived `-watchful` user
exists to avoid Web Service session invalidation and is not part of native
auto-configuration. Native live does reject `invalid`, `expired` and `unlicensed`
licences (`native-client.ts`, `#live`; SDK `session.ts`, `_startStream`). Retain
read-only licence diagnostics and explicit errors; do not alter licensing.

The minimum inventory is the exact shape in `native-sdk.ts`:

| Entity | Required fields and interpretation |
| --- | --- |
| Server | `name`, `serial`, `mac`; version and licence diagnostics separately. NVR external identity is `serial || mac` in `core.ts`, `#ensurePrimaryServer`. |
| Physical device | `id`, `name`, `model`, `ip`, `enabled`, `alarms`, optional `connected`, and its own audio array. |
| Camera | `id`, explicit `deviceId`, `name`, `model`, `ip`, native `format`, optional `connected`. |
| Quality stream | `id`, `parentCameraId`, `enabled`, codec name, native `format`, `width`, `height`, `framesPerSecond`. Main stream normally has `id == parentCameraId`. |
| Audio component | `id`, `name`, `type`, `format`, `frequency`, `bitsPerSample`, `samplesPerBuffer`, nested under the owning physical device. |
| Group | String `id`, `name`, `description`, nullable `parent`, items with numeric `id`, `type` and string `guid`. Camera item type is 1; subgroup type is 4 and uses its GUID. |
| Licence | `status`, `enterprise`, `ipCameraSlots`, nullable ISO expiry; statuses are invalid/expired/unlicensed/trial/restricted/licensed. |

Native format 0 is JPEG, 5 is H.264, 6 is H.265 (`src/native.ts`, `streams`).
Web Service H.265 format is 7. Keep wire, SDK and Web Service enums distinct;
`core.ts`, `#fetchCameras`, explicitly converts 6 to 7 for its shared view.

Channel and device IDs are positive unsigned 32-bit values, not ordinal camera
numbers. Never infer audio ownership with `id / 0x10000`: that HTTP fallback
collapses distinct devices in the observed native inventory. Use `deviceId` and
the device's audio list (`core.ts:282`, `#fetchAudioStreams`; line 293,
`#fetchDeviceTree`; line 629, `getDevices`; SDK `native.ts`, `cameras`).

The SDK currently waits up to one second for connection state callbacks during
`collectInventory` (`src/native-inventory.ts:216`). Unknown connectivity is not
proof of a live camera. Keep enabled/configured/connected separate. A camera
marked enabled but disconnected stays discoverable and probes unreachable;
disabled cameras are excluded. Reachability combines camera state and owning
device state (`core.ts`, `#seedReachability`; `stream-state.ts`,
`cameraReachability`). Refresh per session/probe rather than treating a login
document as permanent health truth.

## Mapping into Watchful devices

The NVR integration is server-scoped. The VMS integration creates one Watchful
device for a selected Exacq camera group. `buildIndex` offers groups with direct
camera items and a synthetic `__all__` “All Cameras” entry whenever monitorable
cameras exist (`core.ts:211`). `#cameraScope` uses direct type-1 group items;
the existing contract does not recursively expand subgroup membership. A missing
selected group returns an empty camera set rather than all cameras.

`getDevices` emits a camera external ID containing `{serverId, cameraId}` and,
when available, `{audioId, audioFrequency}`. Its `videoProcessorId` is the camera
ID as a string. Live streaming can select a child quality stream without
changing that stable camera identity. `ExacqNativeClient.#live` picks the smallest
enabled H.264/H.265 stream by image area. It does not apply the HTTP adapter's
HEVC-first ordering and does not reconfigure camera encoding.

Standalone speakers are also required: `#appendStandaloneSpeakers` (`core.ts:688`)
adds a connected network device with no cameras and an audio output, only for
server/All Cameras scope. Such entries have `model: "Speaker"`, the device ID in
`externalId.cameraId`, and the audio ID as `videoProcessorId`. They must not
appear in every group or be included in motion logging. Cameras with a speaker
remain cameras and must still receive motion configuration.

Existing lab claims cover a groupless recorder. Nonempty/nested group mapping
needs fixtures and real group verification; the implementation should not claim
recursive group semantics without adding and testing that behaviour explicitly.

## Legacy SDK automatic configuration: motion history

The audit below describes the SDK compatibility backend. The pure-Go `wire`
backend now provisions recorder-originated callbacks instead, with no discovery
poll, event retry queue, or persistent native subscription. Its native API,
owner mapping and measured delivery limits are in [webhooks.md](webhooks.md).

The active native plan/apply change is **`native-events.motion`**, with setup type
`native-motion` (`core.ts:1130`, `getAlertingSetupTypes`; line 1185,
`getAlertingConfiguration`; line 1225, `applyAlertingChanges`;
`native-client.ts:204`, `plan`; line 235, `apply`). It comprises recorder-side
motion logging and ihub's persistent polling configuration. A recorder-only write
is not complete platform configuration.

`ExacqNativeClient.eventOwner` derives a stable owner UUID from the first 32 hex
characters of SHA-256 of `watchful-exacq-eventlog:<Watchful NVR/group id>`.
`#alerting` deduplicates camera IDs, excludes entries with `model == "Speaker"`,
and requires a Watchful device identity. The event-link association UUID is the
first 24 hex characters of the owner's unhyphenated UUID plus the camera ID as
eight lower-case hex digits (`motionLogGuid`). Preserve this ownership scheme
when interoperating with existing records.

Every desired camera has an owned link with:

| Field | Required value |
| --- | --- |
| Source/source ID | Motion source 5, main camera ID |
| Target/target ID | 0 / 0: log-only, no action |
| `log` | true |
| Schedule | Exactly 168 hourly values, all 3 (always active) |
| Association UUID | Deterministic owner/camera UUID above |
| Pre/post/extra/threshold | Existing writer uses zero |

`validateMotionLogging` accepts 0–1024 unique positive uint32 camera IDs and a
UUID owner. An empty list removes only this owner's links. Before mutation,
verify every requested camera exists and reject any duplicate managed UUID or
owned-prefix UUID used by another source/action. Preserve unrelated event links.
Update by existing link ID and preserve the association UUID; deletion also
requires the association UUID on observed firmware. Read back status, then
verify persistence with a **fresh authenticated session**. A success return code
alone is insufficient; the SDK README records initial-password-change state
where writes returned zero without persisting.

This does not configure camera VMD regions/sensitivity, camera recording schedules,
NTP, accounts or codecs. Those are not mutations in the active platform native
plan. Camera motion detection and retained footage must already exist for useful
events and snapshots. Expose diagnostics when they do not, and coordinate any
test-only setting changes with the parent/device owner.

## Legacy SDK events, durability and shared snapshots

The required event API is `searchEvents({startMs,endMs,sources:[{type,id}]})`,
returning `{sourceType,sourceId,startMs,endMs?}` intervals. The existing SDK allows
source types 1 and 5, at most 1024 sources, a maximum one-hour window and 10000
results. Unknown/invalid end times remain absent, preserving ongoing motion.
Partial results must not become success after a timeout or transport failure
(`src/native-history.ts:33`, `validateHistory`; line 126, `collectHistory`).

`native-events.ts` owns platform delivery rather than the media producer:

1. Schedule discovery every 10 seconds. Persist cursor and deduplication state
   under a device-scoped owner; coordinate discovery/configuration with a mutex.
2. Query source type 5 for configured cameras, from cursor minus 60 seconds to
   recorder discovery wall time minus 5 seconds, bounded to one hour per query.
3. Deduplicate by `<camera>:motion:<startMs>`, retaining identities while intervals
   overlap the query, including ongoing motion.
4. Enqueue at most three durable one-shot delivery jobs **before** moving the
   discovery cursor. Do not advance past a backlog or a failed search/enqueue.
5. Delivery rechecks camera membership and fetches a snapshot at the event time.
   Emit `CameraMotion`, camera-string `videoProcessorId`, seconds timestamp and
   `requiresObjectClassification: true`, retaining the stable millisecond event ID.
6. ihub/backend owns final acknowledgement after downstream acceptance/filtering.
   Snapshot failures remain retryable; replay can repeat an event ID. This is
   retryable delivery, not exactly once.

`drainEvents` is diagnostic callback delivery, currently bounded to 256 events
with a dropped count (`session.ts`, `open`, `drainEvents`). It is not a substitute
for durable history. Only CameraMotion is mapped by the active native platform;
other callback event types must not be advertised as mapped alarm coverage.
The legacy HTTP webhook path emits `Analytic` events. The wire backend now
parses its own verified motion payload into `CameraMotion` through the ordinary
webhook pipeline; SDK mode retains its existing history path.

Snapshots have separate live and historical contracts. Default width is 1280;
the other supported preset is 1920. Historical capture searches from one second
before the request to three seconds after, chooses the first frame at/after the
request, and returns its actual `capturedAtMs`. Missing footage fails; it must not
substitute a live frame. The platform rejects placeholder images no larger than
2 pixels (`native-client.ts:127`, `snapshot`; `src/archive.ts:40`,
`captureSnapshot`).

**Native JPEG snapshot conversion is now implemented via optional local FFmpeg; see [control-research.md](control-research.md#optional-jpeg-snapshots) for exact timestamp semantics, limits, and three-camera observations. This is downstream decoding of native compressed video, not a proven recorder-side JPEG request.**
The SDK implementation calls `EVAPI_EnableDecompression(1)`, `EVAPI_SetFrame`
for archived footage, and `EVAPI_GetJpegSizedEx`. Those are SDK-local operations.
Determine experimentally whether a native recorder request can produce JPEG;
otherwise explicitly use an existing external decoder path such as FFmpeg or
report that the pure Go source supplies compressed frames only. A pure Go wire
client does not imply a pure Go H.264/H.265 JPEG decoder.

## Historical SDK webhook audit and scope limits

The SDK also exposes webhook target/route and event-link inventory plus
`getAlertingStatus`/`configureAlerting`. This is a separate, currently unused
native platform path (`native-sdk.ts:95`, `ExacqAlertingInventory`;
`src/alerting.ts:10`, `getAlertingStatus`; line 37, `configureAlerting`).
If implemented, match explicit target/route UUIDs, HTTP(S) origin/path/query,
method 2, 3000 ms timeout, and motion links with source 5/target 10. Preserve
unrelated routes/links and existing association UUIDs; verify after fresh login.
Inventory exposes secret presence, not secret values. The current configuration
uses no signing secret and no custom headers. These historical SDK details
are not the current wire implementation: wire mode uses owner-scoped native
webhook provisioning with an Authorization header and five-second timeout.
It needs recorder-to-receiver HTTP(S) reachability, while recorder control and
media still share one forwarded native port.

| Capability | Existing contract and required claim boundary |
| --- | --- |
| Talkdown | Required by `getCapabilities` (`core.ts:447`) and `sendTalkdown` (`core.ts:875`). Select an explicit device-owned output with `(type & 4) != 0`, native PCM `format == 0`; convert source audio to mono signed 16-bit little-endian PCM at the declared frequency. SDK `audio.ts:9`, `sendTalkdown`, validates 8–48 kHz, at most 30 seconds, uses `samplesPerBuffer`, paces complete buffers, zero-pads the last one and keeps its child session open through the final buffer. Accepted bytes do not prove audible output. |
| Incoming audio | Inventory records inputs, but `ExacqNativeSdk.media` and the present producer carry video only. No current incoming/recorded audio track parity claim; add explicit codec, timing and A/V sync tests before supporting it. |
| PTZ | No callable PTZ API or PTZ capability in these integration entry points. `types.ts:154`, `ExacqCameraConfig`, has optional `ptz` metadata; Swagger has broader vendor endpoints. That does not establish a platform PTZ requirement or implementation. Report discovered support separately. |
| Camera encoding | Read stream choices and select an existing compatible stream. Native `#live` does not change codec, resolution, frame rate or quality. Unsupported codecs need explicit failure or a proven decoder/transcode path. |
| Recorder administration | Read identity, version, health and licence. User management is an HTTP-mode workaround; native account creation, licence changes, firmware updates and arbitrary vendor settings are outside existing native auto-configuration. |

## Recommended Go and source interfaces

These names are recommendations, not declarations of implemented API. Coordinate
any public names with the parent SDK producer to avoid scheme/control collisions.

```text
exacq://user:password@recorder[:22609]/<camera-id>
exacq://user:password@recorder[:22609]/<camera-id>?stream=<stream-id>
exacq://user:password@recorder[:22609]/<camera-id>?start=<unix-ms>&end=<unix-ms>&speed=1
```

Keep the path as stable main camera identity and `stream` as an optional selected
quality stream. Auto-selection should use inventory and match the native
adapter's smallest compatible enabled stream. Playback uses recorded camera
identity; reject a secondary-stream request unless wire behaviour is proven.
Use positive uint32 IDs; validate host, port, credentials, bounded time range and
finite positive speed. Treat `start` as playback selection and require `end`
initially to preserve the existing one-hour bounded contract. If RFC 3339 is
accepted for consistency with Milestone, normalize to recorder-clock milliseconds.
Do not promise TLS or invent `exacqs`/`exacqx` semantics before negotiating and
verifying the actual native security modes. Redact credentials in every source
description, API result and error path.

Separate a context-aware native `Client` from the go2rtc `Producer` adapter:

- `DialContext`, `Close`, `Inventory`, `License` and server diagnostics.
- `Live(ctx, streamID)` and `Playback(ctx, cameraID, range)` returning access
  units with codec/parameter sets, source time, keyframe and verified DTS/PTS
  semantics. Limits, cancellation and control/media demultiplexing live here.
- `SearchEvents`, `EventLinks`, `MotionLoggingStatus`, `ConfigureMotionLogging`
  with explicit owner and camera set; mutations never happen during a media dial.
- `Talkdown` with explicit audio ID and PCM contract when proven; snapshot
  capability separately distinguishes recorder JPEG from external decode.

An internal control handler can expose inventory, history and motion plan/apply
using these same methods; keep it independent of streams' reconnect lifecycle.
Prefer named recorder configuration or credential-bearing request bodies over
credentials echoed in generated URLs. Return per-operation capability/error
status so unsupported snapshot/PTZ/audio paths cannot masquerade as success.
The implemented error envelope, codes, HTTP mapping, `diagnose` action and
request diagnostics are specified in [errors.md](./errors.md).
Leave ihub's durable scheduler, Watchful IDs and delivery acknowledgements in
ihub; go2rtc need only supply history/configuration/media primitives. Do not edit
the parent's ihub or `pkg/exacqsdk` working trees to wire this in without coordination.

## Reuse from the native Milestone producer

Use `pkg/milestone`, not `internal/webrtc/milestone.go` (the legacy WebRTC path).
The useful interfaces already exist:

| Reusable piece | Reference and caveat |
| --- | --- |
| Scheme registration | `internal/milestone/milestone.go:8`, `Init`, registers `streams.HandleFunc`. |
| Connection metadata and track probing | `pkg/milestone/producer.go:44`, `stream.init`; line 58, `addTrack`. Probe until decodable keyframe and build H.264/H.265 codec from parameter sets. Adapt Exacq's cached untimed keyframe handling. |
| Annex-B to AVCC | `pkg/h264/annexb`, `EncodeToAVCC`; both H.264/H.265 use the AVCC length-prefix representation expected by raw tracks. |
| Explicit sample timing | `pkg/core/sample.go`, `SampleTiming`, `SetSampleTiming`. Units are codec clock ticks, with unwrapped decode time, nonzero duration, unsigned composition offset and one source clock ID. This metadata stays local to raw packets; RTP payloaders create network headers. |
| Finite EOF and backpressure | `pkg/milestone/playback.go:113`, `accept` sets receiver `Lossless`; line 157, `IsFinite`; line 159, `Start` ends receiver; line 355, `Stop`. `internal/streams/producer.go:173` avoids reconnecting finite producers even on failure. |
| Interruptible pacing | `pkg/milestone/playback.go:323`, `emit`; line 344, `sleep`, select on context cancellation rather than uninterruptible long waits. |
| Picture order parsing | `pkg/milestone/picture.go:38`, `pictureParser.parse`, supports the observed progressive H.264/H.265 subset; one latest SPS/PPS, H.264 POC type 0 for B-frame ordering. Reuse only after verifying format and semantics; not a universal decoder. |
| Reordering | `pkg/milestone/timing.go:37`, `newReorderer`; `gopClock` and producer `writeTimed`. These are currently unexported and tied to Milestone frame/timestamp assumptions; a shared extraction needs parent coordination and regression checks. |

Exacq, like Milestone's ImageServer, stamps pictures in decode order, so the
producer reuses Milestone's POC reordering (`pkg/reorder`) for B-frame streams
with no configuration. An earlier explicit fixed-GOP mode (`reorder=gop&gop=N`)
recovered exact per-picture PTS but required operators to know the encoder GOP
length; it was retired. Evidence: [B-picture timing](b-pictures.md).

Milestone deliberately **closes recording gaps** (`playback.go`, `duration` and
`maxFrameGap`). Existing Exacq playback preserves wall-clock gaps. Keep real
recording gaps in both media time and pacing; do not let idle timeouts terminate
a valid silent range. Cancellation must still stop immediately during a gap.
Deduplicate overlapping search-window boundary frames without discarding valid
reordered frames. Report the actual preceding keyframe start for playback;
do not claim the requested timestamp as the first encoded picture's capture time.

## Evidence and acceptance matrix

The SDK README records checks on recorder 26.1.7.3 and library 1.47.27.1 with an
Axis Q1728 and C1310-E MkII. Those checks establish a reference implementation,
not pure Go support. Later native tests established the constrained B-picture
contract and detected a test chirp through the speaker's internal microphone;
see [native validation](validation.md). That acoustic result does not establish
room coverage or intelligibility. Nonempty group hardware validation and broad
firmware coverage remain unverified.
Record the native implementation's observations independently, including recorder
version, camera model/firmware, codec/profile, resolution, frame rate, GOP/B-frame
settings and exact test duration. Keep credentials and raw login captures out of
fixtures and reports.

| Area | Minimum useful tests and evidence |
| --- | --- |
| Protocol/auth | Sanitized negotiation/login exchanges, wrong password, cancellation at connect/auth, missing/oversized/truncated messages, fragmented/coalesced TCP reads, compressed/transform limits, request correlation, peer close, recorder restart. Fuzz framing/XML/media parsers after actual layouts are known. |
| Session isolation | Two same-account live streams plus archive/history/snapshot/control through the same forwarded port; releasing one operation leaves others alive; cancellation has bounded latency; connection/resource counts return to baseline. Report measured recorder session limit/errors. |
| Inventory | Real camera/stream/device/audio IDs, disabled and disconnected cameras, explicit audio ownership, no-camera standalone speaker, two cameras of different vendors, multistream mapping, empty and nonempty groups, missing group behaviour. Test synthetic ID collisions that defeat arithmetic ownership. |
| Configuration | Plan/no-op/apply, apply twice, add/remove/empty camera set, foreign links preserved, conflicting owner UUID rejected before writes, association UUID preserved, fresh-login readback, denied permission and partial failure. Coordinate all lab writes first. |
| Event history | Stored motion start/end and ongoing interval, overlapping windows, duplicate responses, partial/error result rejection, restart/backlog replay, >3 discoveries, enqueue/snapshot failure, camera removal. Verify actual event-time image and stable IDs through ihub before claiming end-to-end delivery. |
| Live codecs | Concurrent Axis and Dahua where available; H.264/H.265 independently and mixed, primary/substream, cached timestamp-zero keyframe, SPS/PPS/VPS retention, offline camera, midstream codec/resolution change. Decode recorded output and check frame counts/errors. |
| Playback | Seek inside GOP and gap, initial no-footage, near database edge, multiple segments and duplicate boundaries, exact end/EOF, 15-second or longer real gap, cancellation during gap, 0.5x/1x/2x if speed is implemented, H.264/H.265 B-frame and variable cadence evidence. Compare source order, DTS/PTS and decoded frame hashes. |
| Snapshots | Live and historical on each codec, 1280/1920 presets, first frame at/after event, missing footage error, bad/placeholder image rejection, actual capture time. State whether recorder or external decoder produced JPEG. |
| Audio/PTZ | Output inventory and paced PCM buffer acceptance, cancellation, wrong output/rate/input-only rejection, independent audible verification. Incoming audio and PTZ remain explicitly unimplemented unless separately captured and exercised. |
| Performance/soak | Record startup-to-first-decodable-frame p50/p95, end-to-end visual/capture latency where clocks permit, delivered FPS/bitrate, CPU/RSS/goroutines, reconnects/errors/drops, concurrent viewer count and test duration. Soak mixed live plus periodic archive/events, then disconnect/reconnect. Compare SDK and Go using equivalent settings. |
| Build/consumer compatibility | `CGO_ENABLED=0` linux/amd64 and linux/arm64 build; package/race/fuzz tests as appropriate; actual RTSP and MP4/HLS consumer decode. Browser H.265 support is separate from successful HEVC passthrough. |

Do not label SDK-local ABI parsing, mocked tests, single-vendor runs, test alarms,
or accepted audio bytes as wire, broad-compatibility, physical-motion or audible
validation. Report what ran and retain reproducible sanitized evidence.
