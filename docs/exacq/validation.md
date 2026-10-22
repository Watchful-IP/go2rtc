# Native Exacq validation — 21 October 2026

All recorder traffic used multiple sessions through the same forwarded native
port. The native implementation and test server did not load libevAPI or use
the concurrent SDK gateway. Separate SDK work supplied independent behavioral
comparisons; its producer yielded the same final gap-frame count, and isolated
SDK oracles verified authentication and generated media inputs.
No production deployment, merge, license/MAC change or parent-worktree edit was
performed. The parent restored all three original motion-only schedules and
retained the authorized camera additions and codecs.

## Mixed-camera compatibility and sustained capture

The native producer fed go2rtc fragmented MP4 directly. Each file was checked by
ffprobe with frame and packet counting and fully decoded by FFmpeg. All counts
matched, with zero decoder errors and zero overlapping samples. Frame durations
came from the next native timestamp; configured FPS was not used for them.

| Camera | Native stream | 120-second packets = decoded frames | Bytes | First HTTP byte | Largest HTTP receive gap |
| --- | --- | ---: | ---: | ---: | ---: |
| Axis Q1728, 459264 | H.264 1920×1080 | 1,800 | 3,659,335 | 2,174 ms | 96 ms |
| Axis P1468-LE, 459776 | H.265 3840×2160 | 1,800 | 22,570,274 | 1,317 ms | 115 ms |
| Dahua DH-IPC-HDW3849H-AS-PV, 4784640 | H.265 3840×2160 | 2,763 | 103,944,461 | 1,035 ms | 177 ms |

Total: **6,363 packets = 6,363 decoded frames**, about 8.68 Mbit/s aggregate file
throughput across the three concurrent streams. Observed interframe intervals
were 66/67 ms for Q1728, 40/80 ms for P1468 and 20–88 ms for Dahua in this soak.
Earlier native Dahua capture also showed 83/3 ms. Using a nominal configured
frame duration would overlap samples for these streams and drop frames.

The go2rtc process had approximately 26 MiB RSS and 0.8–1.4% CPU in two samples
during capture on the local Apple Silicon host. These are observations, not
profiled peak-resource or capacity guarantees. FFmpeg validation ran afterward.
The reproducible capture runner is [lab-soak.mjs](lab-soak.mjs); it requires
three preconfigured local source names, not command-line recorder passwords.

Additional sequential live and ten-second archive captures for all three cameras
also decoded cleanly; see [producer-validation.md](producer-validation.md).
Short native frame-age probes measured Q1728 median 78.4 ms/p95 79.2 ms and
P1468 median 256.7 ms/p95 299.2 ms relative to recorder timestamps. Dahua timestamps
were ahead of the local clock, so no latency number is claimed for that camera.
Frame age and startup wait are not glass-to-glass latency measurements. The
producer's exact-duration lookahead adds one frame of buffering.

## Qualified B-picture mode

> Historical record. The explicit fixed-GOP mode described here was retired in
> favour of automatic POC reordering shared with Milestone; see
> [producer.md](producer.md#b-frames).

Generated fixed-GOP H.264 and HEVC were registered as a temporary recorder
input in a coordinated window. A guarded in-place speaker disable made the
slot available; existing camera IDs, credentials and recording schedules were
preserved, and no license/MAC data was modified. This is a controlled synthetic
input test, separate from the three physical-camera soak.

Live and archive captures establish a complete-GOP presentation timestamp
multiset for both codecs. The variable-timing source alternates 40 ms and
93⅓ ms intervals. Six native-live and six native-archive GOPs per codec matched
it within 0.667 ms. Actual SDK callbacks independently supplied the same NAL
bytes and picture kinds: 66 H.264 callbacks and 65 HEVC callbacks, all matching
native counterparts. Three complete SDK GOPs per codec matched the same
timestamp multiset. Separate wall-clock windows do not establish equal absolute
timestamps between sessions. See [SDK input equivalence](sdk-input-equivalence.md).

`reorder=gop&gop=N` now enables the bounded implementation. `N` must be the
known fixed encoder count, not learned from the received GOP. The shared AVCC
helper reconstructs PTS by POC and synthesizes increasing DTS no later than
PTS. It supports progressive single-VCL closed IDRs, with H.264 POC type 0
and guarded HEVC syntax. Stable parameters, exact count, dense unique POC,
unique timestamps and a subsequent IDR are all required. One GOP is buffered;
retained GOP span is at most ten seconds, with inter-GOP gaps preserved.

Whole-GOP finite playback searches at most ten seconds beyond the requested
end and reports `ActualEnd` / `actual_end` at its exclusive sealing boundary.
It can include final-GOP pictures beyond the requested end. Default I/P playback
keeps its strict end. Motion cuts and EOF without closure fail explicitly.
Historical snapshot dependencies can extend beyond their selection window,
but the selected recovered PTS cannot. [B-picture timing](b-pictures.md) records
the counterexample that makes the independent GOP count necessary.

Final H.264 and HEVC live MP4 runs each decoded 90/90 pictures; their finite
speed-4 archive runs each decoded 45/45 and reached EOF. Every decoded pixel
hash matched the known 45-picture source cycle in display order, with zero
decoder errors. Live PTS steps were 40/93/94 ms. Positive composition offsets
occurred in 30/15 H.264 live/archive samples and 42/21 HEVC samples.

H.264/HEVC probing took 1,417.2/1,553.5 ms; five seconds of reading plus startup
took 6.418/6.556 seconds in the capture helper. Archive completion took
763/811 ms, including 37.8/85.1 ms of probing. The whole-GOP ends extended the
requested end by 589/590 ms and covered 3,026/3,027 ms of source time. Historical
snapshots for both codecs selected the expected display picture and matched its
independent MP4-decoded JPEG pixels exactly. Two H.264 hardware searches also
confirmed repeatable fractional start/end preparation. These are bounded
generated-source results, not arbitrary B-picture compatibility or glass-to-glass
latency. See [producer validation](producer-validation.md) for the full table.

## Recording gaps, seek metadata and finite EOF

The controlled Q1728 query `[1792543920000,1792543970000)` returned **526 frames**,
starting at `1792543918432` and ending at `1792543969965`. A 16,600 ms timestamp
gap was retained as a 4.150-second sample at speed 4. Export ended normally after
12.96 seconds, with 12.892 seconds of MP4 media, no DTS regressions or sample
overlaps, and clean decoding. The independent SDK implementation produced the
same 526 frames after its own preroll fix.

The observed extra empty 6/3 boundary record is excluded from result Count.
Skipping that exact record fixes a real subsequent-search failure. Tests retain
errors for actual media before metadata and malformed/unknown stream responses.
The nearest preceding keyframe is selected consistently by preparation and the
producer. Reusing its fractional-millisecond timestamp does not return another
older GOP. Starting at `1792543938000` or `1792543942000`, inside the gap, advances
to the first available keyframe `1792543947365`. Empty search windows do not
prematurely become whole-clip EOF. There is no silent live fallback.

Accelerated/irregular MP4 validation uses:

```sh
ffmpeg -v error -i capture.mp4 -map 0:v:0 -fps_mode passthrough \
  -enc_time_base 1:90000 -f null -
```

The explicit time base avoids the null muxer's default coarse timestamp rounding
being mistaken for an input DTS defect.

## Reconnect and viewer lifecycle

[lab-reconnect.mjs](lab-reconnect.mjs) opened its own loopback relay, interrupted
only its own sockets for **1,602 ms**, and then restored them. The same HTTP
viewer continued with a **3,268 ms** maximum receive gap; 313 packets decoded
into 313 frames with zero errors. A fresh viewer reopened the same registered
source successfully (75/75). Canceling one of two viewers after four seconds
left the other receiving through ten seconds (60/60 and 151/151 respectively).
Four upstream sessions were accepted during these checks. The test relay closed
all its sockets and listener on completion; no recorder configuration changed.

## Inventory, automatic motion configuration and snapshots

Native inventory/status verified four physical devices, three cameras, five
video streams, and explicit audio ownership, including the standalone C1310-E
speaker. Read-only license health verified trial status, enterprise capability,
four camera slots and expiry `2026-12-04T00:00:00Z`. License key bytes were neither
retained nor returned. Subsequent temporary parent, nested-camera and audio-only
groups passed through the compiled pure Go VMS path, including scoped motion
configuration and exact cleanup. See [groups.md](groups.md) for evidence and limits.

Three consecutive owner-scoped create → idempotent apply → delete cycles passed
in 0.34/0.27/0.27 seconds. Planning and persistence checks use fresh authenticated
sessions, avoiding stale unsolicited config replies. A dedicated UUID isolated
the test links. Development duplicates were removed by exact ID/GUID; after the
final positive event test, **five fresh sessions each verified zero owned test
links**. No unrelated links were present or altered. Fixtures cover preserving
unrelated and unrecognized actions.

The authorized Axis VMD test alarm produced positive native motion history.
This is a camera-generated test alarm, not proof of physical motion detection.
Ongoing events use recorder EndTime=0 and therefore omit the API end timestamp;
repeated ongoing intervals retain the existing adapter's stable-event-ID and
durable cursor/deduplication rules. The API and compiled ihub adapter retrieved
positive records with the SDK gateway stopped.

All six live/historical JPEG checks returned 1280×720:

| Camera | Live completion | Historical completion | Historical frame after requested time |
| --- | ---: | ---: | ---: |
| Q1728 | 770 ms | 75 ms | 32 ms |
| P1468 | 3,318 ms | 404 ms | 2 ms |
| Dahua | 814 ms | 385 ms | 30 ms |

Historical requested time was `1792543920000`. A Q1728 snapshot at
`1792543940000`, inside the recording gap, returned explicit no-footage in 2 ms.
FFmpeg only decoded the native GOP selected for the requested event time.

The authorized C1310-E output test sent exactly 300 ms of all-zero mono PCM16 at
8000 Hz: **4,800 input bytes accepted**, 459 ms completion including the padded
final native audio buffer. The dedicated session closed, and the test server had
zero open recorder connections afterward. That silence send did not establish
acoustic reproduction. The
first validation attempt sent no audio because it incorrectly treated acquisition
`Enabled=false` as an output prohibition; static SDK verification and a regression
test corrected this before the single successful silence send.

A separate parent-coordinated acoustic check then sent a 300 ms, −36 dBFS
700→2,300 Hz chirp through the native API. HTTP 200 reported 4,800 accepted PCM
input bytes. The speaker's internal microphone captured 1.76 seconds within a
two-second cap. Signal detection passed with normalized correlation
**0.636357**, against **0.169642** for the reversed-signal control. This is
internal-microphone signal evidence, not a human listening test, room-coverage
measurement or intelligibility assessment. No raw audio was persisted.

The first bounded attempt ended on a readiness timeout and restored settings.
The successful second attempt also restored the exact original Axis audio
settings and the changed microphone parameter; both restoration checks passed.

A companion follow-up against an earlier server build encountered one HTTP502
during live license health. Its underlying error was not retained, so the exact
original cause remains unproven. An initial ten-request follow-up passed, and a
later bounded run of 100 fresh HTTP health calls (50 preceded by inventory)
also passed. Those passes did not establish retained-session stability.

A delayed retained-session test then reproduced a matching failure class on call
20: `exacq: missing or ambiguous license status`. The recorder interleaves
partial 3/2 reports without a Delta flag. A 336-byte report without License
arrived after 9/2 and was mistaken for the requested full status. Separate
observations showed RefreshStatus returning a Servers-only update on 19/20
retained calls. Both readers now wait for a validated license-bearing full
report under the existing timeout and byte/message limits. Malformed or
incomplete license-bearing reports still fail explicitly.

Twenty checks passed after the license fix; ten final retained checks passed
with complete camera/storage/memory status after both fixes. One rebuilt private
HTTP check returned 200 in 30 ms, trial license, four connections, three cameras,
one storage section and memory throttling false. The regression metadata in
`pkg/exacq/testdata/health-partial-status.json` contains no payloads or keys.
Synthetic tests cover interleaving, malformed candidates, fixed deadlines and
capture-buffer limits; the health-stage package/API race tests passed. This fixes the reproduced
failure class without retroactively assigning an unrecorded cause to the first
502 or claiming unlimited endurance.
The final compiled-adapter rerun also passed, discovering three cameras plus the
standalone speaker and validating live JPEG, playback preparation, license health
and history against the rebuilt API with zero SDK providers.

## Automated validation and bounded scope

Focused Go race suites cover protocol/auth, session cancellation/rekey, inventory,
motion ownership/persistence, malformed XML/media, SOCKS remote routing, codec
parameters, sample timing, gaps/EOF, license health, snapshot timing, private API
and shared source redaction. Final race-enabled tests passed for `pkg/exacq`,
`internal/exacq`, `pkg/exacqtiming`, `pkg/core` and `pkg/mp4`; baseline stream
lifecycle tests also passed. Fuzz
targets cover packet framing, CBC/DH/SPAKE envelopes, configuration, status,
motion results, search metadata, media packets, video state and audio payloads.
The independent mu-law oracle checked all 65,536 PCM16 sample values.
The shared timing-buffer fuzz target completed 650,765 executions in ten
seconds without failure. Fixed-GOP regressions cover missing highest-POC
pictures, malformed additional slices, timestamp ambiguity, gap boundaries,
empty-window deduplication and cancellation with queued output.

Mechanism-1 SPAKE2+ is now implemented. Three reproducible known-answer
transcripts call the actual pinned SDK's password derivation, curve and exchange
routines with synthetic credentials; Go matches public points, password scalars,
both proofs and the session key. Wire-state tests cover salt echo, server-proof
verification, strongest advertised mechanism, cancellation and key transitions.
Malformed and noncanonical encodings are rejected. Password-dependent identity
or torsion failures use an opaque proof and are rejected in the server-proof
phase, preventing an early response from revealing password-derived residue.
No invalid peer releases a session key. The original lab still negotiates
mechanism 0. On 22 October an isolated instance of the actual recorder 26.1.7.3
was migrated through the vendor-supported password-strengthening operation.
It advertised mask 3 and pure Go selected mechanism 1: four correct logins,
authenticated configuration, status and licence reads passed. One wrong password
was rejected, followed by a successful correct login. The rebuilt private API
returned HTTP 200 for inventory and three consecutive health calls against both
the isolated SPAKE2+ server and original P224 recorder. This is actual recorder
software evidence, not a synthetic peer. See [spakeplus.md](spakeplus.md) and
[spakeplus-validation.json](spakeplus-validation.json).

The fresh unlicensed recorder exposed a real parser gap: its status omits licence
flags and clock. The fix accepts only the observed five-zero-attribute empty
shape when no key is present; keyed and incomplete licensed responses remain
strict. Status uses the same shape as a full-report anchor. Regression tests
cover malformed attributes and interleaved unsolicited partial updates. The
native client, timing package and private API race suites pass.

Final `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build .` and the equivalent arm64
build both pass with the GOP implementation. There is no vendor binary in
either executable.

The complete repository `go test ./...` is not green: unrelated existing failures
include Tuya nonconstant fmt vet calls, FFmpeg platform/template expectations,
missing HomeKit test types, TLV8 expected separator data, and mDNS interface
availability. These areas were not changed for Exacq; targeted affected suites
pass. These failures were independently reproduced from a clean archive of the
original HEAD; see [test-baseline.md](test-baseline.md).

The isolated ihub companion supplies explicit wire/SDK selection, private API
authentication and body-only control credentials, recorder SOCKS routing,
owner-scoped native configuration, direct source URLs and event-time
snapshots. The baseline 93 focused companion tests, core/integration typechecks,
both NVR/VMS builds and compiled same-thread/worker executions without an SDK
provider passed. Actual
compiled-adapter hardware tests passed against the authenticated local API.
Its README and sanitized validation JSON are in the companion checkout.
The final companion follow-up at `042ffd6a` passed 34 focused tests, the
integration typecheck and both NVR/VMS dual-bundle builds. The separate .NET
host prerequisite and its 104-test result are qualified below.

The final Go framing fuzz run executed 292,260 cases and search-metadata fuzz
executed 45,058 cases without failure. Other focused fuzz results are described
in the protocol research and test sources.

## Cleanup and handoff

After the baseline validation, the native test server, byte-capture relay, its
reverse SSH forward and the isolated fault-injection relay were stopped. The
native process had zero recorder sockets after the quiet audio check. Dedicated
motion test links were absent on five fresh readbacks. The parent independently
restored and verified the original recording schedules. The later temporary
B-picture input window is separate from that baseline cleanup.
That follow-up's temporary device was deleted, and three fresh native readbacks
confirmed the original speaker enabled with its original output intact. A fresh
SDK inspection matched the private baseline fingerprints, all original IDs,
schedules and formats, with four enabled devices and four licensed slots.
The temporary RTSP source and its in-process watchdog were stopped, as was the
test-owned SSH tunnel; both loopback listeners were verified closed. The parent's
subsequent acoustic window completed with its changed settings exactly restored.
The owned native API/test process and capture relay were then stopped. Final
checks found no local listeners on the API, RTSP, temporary source or capture
relay ports, and no temporary source listener on the recorder VM. Protected
shared credential caches were retained for handoff.
Private credentials and lab evidence are retained in protected local
directories for the two implementation threads; none are committed. Both
implementations remain in isolated feature branches, with no merge or deployment.

Limits remain explicit: B-picture timing requires the documented fixed-count,
progressive, single-VCL closed-GOP opt-in; unsealed motion cuts, adaptive/open
GOPs and unknown counts are unsupported. SPAKE2+ is qualified against actual
recorder 26.1.7.3 software and SDK differential fixtures; other versions and
appliances remain unqualified. Incoming
audio tracks, PTZ and generic vendor configuration are not advertised.
Owner-scoped motion webhooks were subsequently qualified; see [webhooks.md](webhooks.md).
The current HTTP contract also exposes no listen or PTZ method. The parent .NET
host's routing/URL changes passed 104 focused tests, including the actual compiled
wire transport in ClearScript; that separate prerequisite is documented in the
companion README. Older unsupported runtimes fail before dialing. A
two-minute soak is not evidence of days of endurance or unlimited session count.

### Group/authentication follow-up cleanup

On 22 October, the temporary group hierarchy and both motion owners were removed
and verified against their saved baselines. The isolated password-strengthened
recorder, namespace, relay and SSH tunnel were stopped; no namespace processes
or mounts remained. The local test API was stopped. A final fresh transport and
authenticated login against the original recorder still reported mechanism mask
1. Its original processes remained unchanged. Only protected private recovery
state and sanitized qualification evidence were retained.


### Recorder-originated webhooks

The 22 October follow-up replaced wire-mode event-history discovery with native
provisioning of recorder-originated HTTP callbacks. Configuration sessions close;
no idle native subscription is needed. Real configuration, idempotence,
two-owner isolation, concurrent setup, motion callbacks and historical snapshots
passed. The final compiled NVR/VMS replay used ordinary webhook parse/snapshot
hooks with zero delivery jobs. Delivery is intentionally best-effort; no custom
retry queue or history backfill is added. See [webhooks.md](webhooks.md) and
[webhook-validation.json](webhook-validation.json) for evidence and boundaries.
Final original configuration matched after excluding clock values and XML order;
test targets, links, local API and VM receiver were removed/stopped.
