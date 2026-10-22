# Native Exacq control and inventory

This document separates recorder observations from static SDK evidence. Addresses
refer to the symbol-bearing Linux x64 `libevAPI.so` 1.47.27.1 shipped in
WebService26.1.4.0. No vendor library is linked or loaded by the Go implementation.
The platform contract is documented separately in [platform-contracts.md](platform-contracts.md).

## Verified recorder behavior

On 2026-10-21, the native Go transport authenticated through the single forwarded
TCP port and requested configuration with packet type 4/subtype 1, empty payload.
The recorder returned 4/2, status 0, with a complete `eDVR` XML document. The first
capture was 57,074 bytes and described one Axis Q1728 camera and an Axis C1310-E
Mk II speaker. Later captures included Axis P1468-LE and Dahua devices added by
the coordinated SDK thread. Plaintext camera credentials occur in device
attributes: complete captures are private files and must never become fixtures.

`ParseConfig` was tested against that private complete response. The committed
`testdata/config.xml` is a reduced, allowlisted fixture: it excludes credentials,
license material, network addresses, serials, unrelated configuration, and
original user-provided names. It preserves the observed IDs and stream settings.

The initial camera mapped to camera/stream CASID 459264, wire stream ID 459264,
H264 format 5, 1920×1080, 15 fps. The output audio entries were explicitly owned
by their devices. Q1728 audio output CASID 459393 mapped to wire ID 459281;
speaker output CASID 459649 mapped to wire ID 459537. Both reported format 0 and
8000 Hz. Format alone does not prove a wire PCM encoding; see audio protocol
research and the separate talkdown implementation.

Status request 3/1 with an empty payload returned 3/2 status 0, 17,703 bytes.
`RefreshStatus` verified four connection states and three connected cameras.
The response included a separate connected state for the speaker, channel
connection states for video, motion/record-idle statuses, storage capacity and
alarm state, and memory throttling. The allowlisted status fixture excludes
paths and licensing material. Read-only native motion history over one hour
completed successfully with zero intervals before the temporary logging test.
The later coordinated positive-event and cleanup evidence appears below.

## Inventory XML and identity

| XML | Meaning | Evidence |
| --- | --- | --- |
| `Servers/Server@Model,Serial,Description` | Authenticated server identity | `ParseServer`, 0x27f890 |
| `Server/Time@UTCTime,Uptime` | Unix seconds and uptime | 0x27fa5c–0x27fab6 |
| `Server/TimeServer@Server` | Configured time source | 0x27f9b7 |
| `Modules/Module@Name,ID` | Plugin name to ID map | `ParseModulesPlugins`, 0x315bd0; actual config |
| `Devices@ID,Description` | Device plugin and fallback model description | `ParseDevices`, 0x28e280 |
| `Device@Number,Enabled,Name,OEM,Serial,Firmware,ipAddress` | Physical device inventory | `ParseDeviceConfig`, 0x27bf50; actual config |
| `Device[/Context]/Video/Input@Number` | Camera input number | `ParseVideo`, 0x283f40 |
| `Input/OSD@Name` | Camera display name | `Camera::ParseOSD`, 0x391090 |
| `Context@Number` | Context number, usually secondary streams | `ParseDevices`; actual config |
| `Codec/Encoder@Number` | Encoder index | `ParseCodec`, 0x2820c0 |
| `Encoder/Channel@Number,Standard,Width,Height,Framerate,Enabled,ID` | Stream identity and format | 0x28265c–0x282904; actual config |
| `Encoder/Sequence/Entry@Position,Input` | Ordered analog input mapping | 0x282209–0x28256d |
| `Audio/Input@Number,Name,Output,SearchOnly` | Device-owned audio endpoints | `Device::ParseAudio`, 0x3959a0; `AudioIO::UpdateInput`, 0x395780 |
| `Encoder/AudioChannel@Number,Standard,Enabled,SampleRate,BitRate,BufSizeSamples,ID` | Audio format and wire ID | `ParseEncoderAudio`, 0x281550; `UpdateStreamInfo`, 0x395280 |

The CASID is `context<<28 | pluginID | deviceNumber<<8 | inputNumber`.
`pluginID` is already shifted; it occupies mask `0x0fff0000`. A physical device
ID omits context/input. A normal video input number is at most 127; bit 7
distinguishes an audio CASID. Without `Sequence`, a codec stream uses input
`(encoderNumber<<4)+channelNumber` (0x283a86). With `Sequence`, channels index
the position-sorted entries, and use the entry's input number (0x282959).

`Channel@ID` becomes `Camera.m_streamID` at internal offset 0x14c. The SDK then
inserts the wire-ID→CASID map at 0x282a62–0x282b1b. These are separate IDs even
when their values happen to match. Stored-search result stream IDs are a third,
request-local mapping; do not reuse live config IDs for playback responses.
Ordinary SDK parent-camera lookup masks CASID with `0x0fffff7f`
(`EVAPI_GetParentCameraId`, 0x36b4f0). The SDK has a special camera flag at offset
0x167 that preserves the original ID; that specialized case has not been
validated and must not be advertised as universal virtual-camera support.

The `Core` module uses signed XML ID `-2147483648`; inventory preserves its
32-bit representation `0x80000000`. Device plugin IDs remain positive and
aligned to 65536. Format codes are SDK codes, including 0 JPEG, 5 H264, 6 H265;
the HTTP integration's HEVC value 7 is not applicable.

Groups recurse as `Groups/Group@Name,Description`, with nested `Group` elements.
Camera membership is nested `Devices/Device[/Context]/Video/Input`, confirmed
by `CasidEnum` 0x255720 and `AddContextItems` 0x255510. `Group::FLoad` 0x2e9460
constructs ordinary IDs by concatenating parent ID, name and `_gr`; map groups
use the Map attribute and `_mp`. Actual parent, nested-camera and audio-only
groups were subsequently qualified on the recorder; see [groups.md](groups.md).
Membership is direct, not a recursive union. Unknown group item classes are not
exposed as camera members.

## Motion logging updates

Static writer evidence: `CConnectionImpl::FUpdateEvt` 0x2d7c10,
`ev::Event::Write` 0x22a4d0, `EventSource::FWrite` 0x2d0060,
`ev::EventSource::Write` 0x221950, `EventTarget::FWrite` 0x2d2df0.

The source enum exposed by the SDK is 5 for motion. `Init::InitStaticMaps`
0x2d73d0 maps that to internal event-source 23. Its schema bytecode at
`s_dnidiitemEventSource` 0x6ca720 and `s_rgitemEventSource` 0x6ca880 is
`23 24 25 26 31 32 33 34 35 36` (hex): Devices/ID, Device/Number,
optional Context/Number, Video/Input/Number/Motion. Context zero is omitted.
The no-action target enum 0 writes no child element.

The exact narrow update form is:

```xml
<eDVR Delta="1">
  <Events ID="655360">
    <Event ID="0" PreAlarm="0" PostAlarm="0" Log="1"
           AssociationGUID="12345678-1234-1234-1234-123400070200" Threshold="0">
      <Devices ID="458752"><Device Number="2">
        <Video><Input Number="0" Motion="1"/></Video>
      </Device></Devices>
      <Schedule><Entry FirstHour="0" LastHour="167" AlarmEnabled="3"/></Schedule>
    </Event>
  </Events>
</eDVR>
```

Send as packet 4/3. Plugin IDs are discovered, not hard-coded. Creation uses
`Event@ID="0"`, matching the existing SDK provider and verified recorder writes.
The SDK also removes a -1 placeholder at 0x2d81a1, but that alternate form is not
used. An update retains the recorder-assigned integer ID. Deletion retains ID
and AssociationGUID and adds `Delete="1"`, with no source, target or schedule
children. Numeric booleans matter: `Motion="true"` was silently ignored by the
recorder even though it is valid XML. An offline invocation of the SDK source
writer confirmed `Motion="1"`; successful native writes then confirmed it on the
recorder. A ClientID is not required for the observed writes.

The recorder normalizes the source to `Video/Input@Context="0"` rather than
omitting context. Parsing recognizes both this attribute and the SDK's optional
Context wrapper. A control write can emit unsolicited configuration responses;
planning another write from an existing session's next full config can consume
a stale response. `ConfigureMotion` therefore plans on a fresh authenticated
session, sends each mutation once, drains broadcasts, and verifies through new
sessions. It never retries creation based on a stale acknowledgement. Three
consecutive create → repeat idempotently → remove cycles passed in 0.34, 0.27,
and 0.27 seconds. Temporary duplicate links discovered during development were
removed by exact dedicated-owner ID/GUID; five fresh snapshots verified no test
links remained. No unrelated links were present or changed.

`ConfigureMotion` mirrors the existing platform contract: validate a UUID owner
and at most 1024 distinct known cameras, derive per-camera AssociationGUID from
the owner's first 24 compact hex characters plus eight camera-ID hex characters,
and reconcile only this prefix's links. It rejects collisions with another
action or duplicate managed identifiers. Other links remain unchanged. It
performs fresh authenticated planning and readback before reporting persistence. An empty
camera list removes only this owner's links. Changes are delta XML, so unknown
settings and device credentials need not be preserved or rewritten.

`EventLink` classification is intentionally conservative: unknown source or
target combinations are marked -1 and cannot be rewritten as managed motion
links. The implementation does not claim generic event-target parity, arbitrary
alerts, SMTP, generic notification, or vendor-wide configuration support.
The separate owner-scoped motion webhook implementation is documented in
[webhooks.md](webhooks.md); it is the wire adapter's primary event path.
Motion history remains an explicit query, not a live event poll.

## Motion history

The SDK uses packet 6/1 and this form:

```xml
<eDVR Delta="1" Type="0" Start="1792540000" End="1792540060">
  <Events ID="655360">
    <Devices ID="458752"><Device Number="2">
      <Video><Input Number="0" Motion="1"/></Video>
    </Device></Devices>
  </Events>
</eDVR>
```

`event_search_header_xml` is 0x2df080; `event_search_sources_xml` is 0x2dee50.
Responses are packet 6/2 with `eDVR/Events`; `Events@Last="1"` completes the
query (`ParseEventsSearch`, 0x278740). An observed positive response was preceded
by a packet 6/3 boundary: exactly 32 bytes, first little-endian uint32 `0x50000`,
all remaining bytes zero. The same exact marker is handled by video search.
Other 6/3 data is not silently accepted as a marker. Motion results are nested
beneath the same source path:

```xml
<Input Number="0" Motion="1">
  <Result BeginTime="1792540000" BeginMilliseconds="123"
          EndTime="1792540002" EndMilliseconds="456"/>
</Input>
```

A coordinated Axis Q1728 VMD `sendAlarmEvent` produced positive native history
for camera 459264. One completed observed interval ran from Unix
1792544359.611 to 1792544361.000. Queries during an active alarm returned
`EndTime="0" EndMilliseconds="0"`; `MotionEvent.End` is nil and omitted from JSON
for this sentinel. Later queries returned its actual end. The server can repeat
identical intervals within one response; the platform's existing camera/start
identity deduplication is still required. After the positive test, five fresh
sessions 150 ms apart verified owned=0 and unrelated=0 links.

`device_context` 0x2e0550 identifies motion by the presence of the Motion
attribute. `common` 0x2df8e0 reads result time fields, followed by optional
EventMetadata. The implementation exposes intervals; it does not expose
unverified metadata filters or other event classes. Queries are bounded to one
hour, 1024 cameras, 10000 received intervals, 2048 packets and 32 MiB cumulative
payload (including unrelated controls). Exact subsecond overlap filtering matches
the SDK provider: an interval's end must be at least the requested start, or
unknown, and its start must precede the requested end. Empty results serialize
as `[]`. Unknown response camera IDs are rejected. Searches have a fixed session
timeout; cancellation or failed finite replies close the connection so leftover
responses cannot contaminate a later query. Public video/event/snapshot requests
reject ranges beyond the signed-32-bit search epoch, including end rounding.

## Status and safety boundaries

`RefreshStatus` requests 3/1 and parses 3/2. It applies only attributes actually
present: `Device@Connected`, `Encoder/Channel@Connected`, `Video/Input@Loss`,
Motion and RecordIdle, plus Storage/Drive and Memory@Throttling. Channel states
resolve through inventory encoder/channel mappings, including Sequence. It
publishes a new inventory snapshot; readers holding an earlier snapshot do not
observe in-place mutation. Unsolicited 3/2 updates can contain only Servers,
Analytics or Users and omit Delta. RefreshStatus waits for a full report marked
by Systems/License, validates that section, then publishes all health fields.
ReadLicense independently waits for a license-bearing report after 9/2; a
partial update cannot substitute for it. The retained-session regression and
bounded hardware verification are documented in [license.md](license.md). Camera and device connectivity use the same CASID
lookup as the existing SDK provider's callback cache. Missing state stays nil.

XML parsing has a 32 MiB byte limit, 64 nesting levels, 250000 nodes and 128
attributes per element. It rejects directives, namespaces, duplicate attributes,
multiple roots and malformed known numeric fields. Errors do not echo XML or
attribute values. The known vendor declaration `<?xml ?>` and trailing NULs are
handled. Unknown config elements can be ignored without exposing their data.
Unit tests cover sanitized observations, independently different CASID/wire IDs,
contexts, ordered Sequence mappings, groups, ownership collisions, updates and
deletes, malformed schedules, bounds and timestamp parsing. Fuzz targets cover
configuration, status and motion-history parsers.

Connections used for control operations should be dedicated control sessions.
The client serializes readers, but callers should not start media and control
queries on the same session and expect a general-purpose asynchronous router.
Every connection uses the original forwarded address; no advertised device or
recorder address is followed for control, history or configuration. Connect has
one timeout budget across dial, authentication and configuration; the full
inventory read ignores Delta broadcasts and counts all payloads against a
32 MiB/128-packet budget. Status has the same cumulative-byte/packet budget and
fixed operation deadline. Synthetic tests verify partial-before-full config,
control traffic that would otherwise extend deadlines, byte-budget exhaustion
and closure of failed sessions. Race tests pass for the native package and its
private control API.

## Optional JPEG snapshots

`Client.Snapshot` obtains compressed video over the native connection and invokes
local FFmpeg solely for JPEG conversion; it has no vendor-library dependency.
The dedicated client is consumed and closed on completion or failure. Live
capture retains parameter sets from an untimed cached picture, then waits for a
timed decodable keyframe. Historical capture searches `[at, at+3s]`, retains the
preceding GOP, and selects the first picture whose timestamp is at least `at`.
No historical result falls back to live. B pictures and reordered timestamps
are explicitly rejected until verified timestamp/display-order support exists.

Output is capped at 8 MiB, video input at 32 MiB, width at 4096, capture at 30
seconds and decoding at 10 seconds (caller cancellation may shorten these).
FFmpeg receives only a raw byte stream, with no URL or credentials. Decoder
stderr is suppressed; missing FFmpeg has a distinct public error. Synthetic
H264/H265 tests verify GOP selection, timed live startup, missing footage,
cancellation, bounds, actual decoded JPEG dimensions and selection of a picture
different from its preceding keyframe.

All three lab cameras produced live and historical 1280×720 JPEGs through the
single forwarded native port. The historical request was Unix1792543920000ms:

| Camera | Live elapsed | Historical elapsed | Selected timestamp offset |
| --- | ---: | ---: | ---: |
| Dahua 4784640 | 814 ms | 385 ms | +30 ms |
| Axis Q1728 459264 | 770 ms | 75 ms | +32 ms |
| Axis P1468 459776 | 3318 ms | 404 ms | +2 ms |

These are individual operation observations including keyframe wait and JPEG
decode, not end-to-end streaming latency percentiles. A Q1728 request at
Unix1792543940000ms, inside the independently created real recording gap,
returned `ErrNoSnapshot` in 2 ms without a live fallback. Images and raw camera
captures remain private; committed media fixtures are synthetic.
