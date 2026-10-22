# Exacq native protocol: static evidence

This document records static findings from Exacq 26.1 binaries. A recovered
structure is called a wire structure here only when sender or receiver assembly
also establishes how it crosses the TCP connection. Runtime verification is
tracked separately; static evidence does not establish server compatibility.

Sources used:

- Linux client `edvrclient`, 26.1.8.0, unstripped symbols.
- WebService 26.1.4.0 `libevAPI.so`, reported SDK 1.47.27.1, with DWARF.
- SDK `Connection.cpp` DWARF compilation unit at decimal offset 11354091.
- Local extraction/annotated-disassembly scripts in `/tmp/exacq-wire`, run using
  `uv run --with pyelftools`. No camera or recorder configuration was changed
  during this static investigation.

## TCP record header

`tagCommHeader` is 32 bytes (DWARF DIE `0xb0f00d`).
`production_connection::FQueueRequest` at `0x2599a0` creates the header and copies
it verbatim before the payload. On the supplied x86-64 binary the integers are
little endian. This is independently consistent with the client's
`SystemImpl::TransformAndSendPacket` at `0x129c100` and
`DataSocket::SendPacket(tagCommHeader&,...)` at `0xbd25f0`.

| Offset | Bytes | Field |
| --- | --- | --- |
| 0 | 4 | `packet_length`, including the 32-byte header |
| 4 | 4 | `packet_type` |
| 8 | 4 | `packet_sub_type` |
| 12 | 2 | `packet_status` |
| 14 | 1 | `compression_type`: 0 none, 1 zlib |
| 15 | 1 | `encryption_type`: 0 none, 1 Rijndael |
| 16 | 16 | Reserved; outgoing SDK requests zero these bytes |

Type values recovered from DWARF: 1 discovery, 2 login, 3 status, 4 config,
5 live, 6 stored, 7 key, 8 PTZ, 9 license, 10 binary, 11 client audit,
12 remote client. Zero is reserved.

Subtypes are specific to the packet type:

| Type | Subtypes |
| --- | --- |
| Generic | 1 request, 2 response |
| Live/stored | 1 request, 2 XML response, 3 stream payload |
| Config | 1 request, 2 response, 3 update request, 4 update acknowledgment, 5 secret request, 6 secret response, 7 OAuth request, 8 OAuth response |
| Login | 1 request, 2 response, 3 token request, 4 token response, 5 negotiate, 6 second reviewer request, 7 second reviewer response, 8 OAuth request |
| Binary | 1 put request, 2 put response, 3 get request, 4 get response, 5 delete request, 6 delete response, 7 put large blob request, 8 put large blob response, 9 get large blob request, 10 get large blob response |

Login statuses include successful 0, server LDAP failure 1, LDAP unauthorized 2,
Kerberos failure 3, not enterprise 4, second reviewer disabled 5, logout success 6,
OAuth failure 7. Error enum values -1 invalid request/response, -2 missing user
accounts, -3 invalid credentials and -4 blocked by schedule travel in the u16
field as their two's-complement representation. Do not assume any nonzero status
means invalid credentials.

Stored search statuses: 0 submitted, 1 not permitted, 2 not bound, 3 start
failure, 4 execution failure. These are packet statuses, not SDK callbacks.

`production_connection::Read` at `0x25ae20` consumes partial records, handles
encryption before zlib decompression, and calls
`CConnectionImpl::Response(type,subtype,status,bytes,size,overflow)` at
`0x2adfd0`. Compression/encryption specifics and authentication belong to the
authentication research; these enum names alone do not identify keys, padding,
or the negotiated authentication mode.

## Stream payload

`tagStreamHeader` is independently recovered at DWARF DIE `0xd3f95a`. It is 32
bytes. For outer types 5 and 6, subtype 3, the decrypted/decompressed payload is:

```
tagStreamHeader[32] | motion metadata[motion_length] | media[remaining bytes]
```

`CConnectionImpl::Response` at `0x2ae1da..0x2ae220` proves this placement: it
reads the u32 at payload offset 12, adds 32, and passes the remainder to
`StreamResponse`. There is no callback pointer or machine-word-sized timestamp
in this wire structure.

| Offset | Bytes | Field |
| --- | --- | --- |
| 0 | 4 | `stream_id` |
| 4 | 4 | `time`, recorder seconds |
| 8 | 2 | `milliseconds` |
| 10 | 1 | `flags` |
| 11 | 1 | `type`, frame/media kind, not codec |
| 12 | 4 | `motion_length` |
| 16 | 16 | Reserved |

`CPacket::CPacket(tagStreamHeader const&,...)` at `0x2f5e60` copies time into
an internal 64-bit field without inventing subsecond bits. `UpdateFrameMode`
at `0x2f9680` calculates `time*1000+milliseconds`. No separate PTS, DTS, duration,
composition offset or frame sequence counter has been established in the
reserved bytes. Preserve the reserved bytes until captures show their meaning.
The Go parser retains raw fields and motion bytes, bounds checks motion length
before indexing, and exposes a zero timestamp for zero-second cached frames.

`CMediaConnection::StreamResponse` at `0x304e50` dispatches type 0 to audio,
type 4 to serial, and other values to video. DWARF enum DIE `0x1605876` in its
compilation unit establishes that header `type` is a frame/media kind:

| Type | Kind |
| --- | --- |
| 0 | Audio |
| 1 | I frame |
| 2 | P frame |
| 3 | B frame |
| 4 | Serial |
| 5 | Subtitle |

The SDK copies this type to `_PSFILE_PACKET.type` in `WritePacketGeneric`
(`0x2fac72..0x2fac76`). `PSConvertAVCodecStream::WritePacket` at
`0x1b956a..0x1b9572` then sets the FFmpeg packet key flag when it equals 1.
Codec selection instead comes from inventory or stored `Result@Standard`.
The first direct pure-Go live capture reported by the runtime agent contained
type 1 for the cached H264 I frame and type 2 for deltas. All those records had
flags `0x80`, so that flag cannot identify keyframes. An earlier working
hypothesis confusing the following codec values with header type was disproved
by this capture; the parser never applied that hypothesis.

The separate codec/PS channel format enum is named by the client
`StreamTypeToName` function at `0x68eed0`, jump table `0x17553c0`:

| Type | Name |
| --- | --- |
| 0x80 | JPEG |
| 0x81 | MPEG1 |
| 0x82 | MPEG2 |
| 0x83 | H263 |
| 0x84 | MPEG4 |
| 0x85 | H264 |
| 0x86 | H265/HEVC (established by the call sites below) |
| 0x90 | PCM |
| 0x91 | ADPCM |
| 0x92 | MP2 |
| 0x93 | MP3 |
| 0x94 | AAC |
| 0x95 | G726 |
| 0x96 | PCM16 |

That name function omits H265. `PSConvertAVCodecStream::EnsureHeaderWritten`
restricts codec to `0x85` or `0x86` (`0x1b90f4..0x1b90fc`), takes the H264 path
for `0x85`, and otherwise calls `ParseHEVCExtraData` at `0x1b912a`.
`PSConvertContainerSource::DoGetNextPacket` also tests codec `0x86` at `0x1cc6be`
before calling `ParseHEVCHasVCL` at `0x1cc6d8`. These establish H265's codec
value, independently of the small frame-kind enum in the wire header.

Audio may have its own nested header; type 0 dispatch does not mean the media
bytes are plain PCM. The meaning of header flags and any end-of-stream bits is
not yet established. Confirm random access from elementary H264/H265 NALs in
addition to the recorder's I-frame classification.

## Camera IDs and stream IDs

A camera CASID is not necessarily a wire stream ID. In normal device-backed
camera requests, SDK `SearchRequest` at `0x297f40` decomposes CASID as:

```
pluginID = casid & 0x0fff0000  // retained in its encoded position
deviceNumber = (casid >> 8) & 0xff
inputNumber = casid & 0x7f
contextNumber = casid >> 28
```

The input kind (video/audio/serial) is obtained using `IdtFromCasid`; do not drop
bit 7 or infer input kind solely from these camera-specific masks.

Live stream mapping comes from configuration parsing. `ParseCodec` traverses
`Codec/Encoder/Channel`, reads `Channel@ID`, constructs a CASID from its device,
input and context, and updates a stream-ID-to-CASID map
(`0x282a62..0x282b1b`). `CasidFromStreamid` at `0x26a8f0` consults that map.
For the ordinary no-Sequence path, input is `(Encoder.Number << 4) +
Channel.Number` (`0x283a86`), not simply `Channel.Number`. The configuration
parser research owns alternate Sequence mappings.
Stored stream mapping instead also uses `Result@StreamID` from search responses.

## Live subscription

SDK `StreamRequest(bool add, uint32 casid)` at `0x297df0` builds a document with
an XML declaration and one of these roots, and sends type 5/subtype 1:

```xml
<Add><Stream id="459264" ref="1"/></Add>
<Drop><Stream id="459264" ref="1"/></Drop>
```

`id` uses the supplied CASID. `ref` is 1 when the connection mode member at
offset `0x380` is zero and 0 otherwise; its broader semantics are not proven.
There is no `eDVR` wrapper in this SDK request. Batched request equivalence and
server behavior for duplicate add/drop need runtime tests. `ref` must not be
documented as a frame reference count without evidence.

Type 5/subtype 2 is parsed as XML. Contrary to an initial hypothesis, its
`Parse(bool live,...)` path skips stored search-result parsing; live mapping
should be derived from inventory rather than assumed to arrive in this ack.

## Stored video search

SDK `SearchRequest` builds type 6/subtype 1:

```xml
<eDVR Start="1700000000" End="1700000010" Type="1">
  <Devices ID="458752">
    <Device Number="2">
      <Video><Input Number="0"/></Video>
    </Device>
  </Devices>
</eDVR>
```

The numeric example uses CASID 459264. Times are seconds. Type 1 is video,
Type 0 metadata; verified against virtual `VideoOrMetaSearch` at vtable slot 3.
It is not the separate `SearchType` enum (normal=2, frame=8).
If context is nonzero, wrap `Video` with `<Context Number="context">` beneath
`Device`. Optional root `Resolution` is included when `GetInterval()>0`.
`SearchRequestStream` at `0x268ff0` chooses Video/Audio/etc from the input kind.
Frame-search subclasses can add children to `Input`; a snapshot request must
not guess those children.

Type 6/subtype 2 XML is parsed by `FParseSearchRoot` (`0x27abe0`) and
`ParseSearch` (`0x278250`). Video hierarchy:

```xml
<eDVR Type="1">
  <Devices ID="458752"><Device Number="2">
    <Video><Input Number="0">
      <Result Start="..." End="..." Count="..." StreamID="..."
              Standard="..." Width="..." Height="..." Flags="..."/>
    </Input></Video>
  </Device></Devices>
</eDVR>
```

`Start`, `End`, `Count`, `StreamID` and `Standard` are required for the SDK to
accept a Result. Video additionally requires `Width` and `Height`. Audio
results use `SampleRate` and `BufSizeSamples`. Flags are optional. The parser
normalizes signed `StreamID` using its absolute value. Multiple Results are
retained and their Count values added. Missing/gapped intervals must not be
silently filled or collapsed into continuous recorded coverage.

The SDK parses Start/End/Count/StreamID with `QueryIntValue(int*)` and
sign-extends Start/End (`0x27836f..0x278408`). The search parser follows that
signed 32-bit XML contract; unsigned wire seconds do not establish post-2038
search XML behavior. The server's observed `<?xml ?>` declaration and optional
trailing NULs are normalized before strict XML decoding. Required identifiers
and malformed results produce errors rather than accidental empty-search EOF.

The SDK's search-complete callback 13 is not wire packet type 13.
`CMediaConnection::SearchResponse` (`0x2f6930`) stores the aggregate count and
result range. `FStoredSearchComplete` (`0x2f4f20`) considers stored acquisition
complete when received video+audio packet count equals the expected count or a
received header's seconds exceed the requested end. `CheckSearchComplete`
also waits for metadata/serial pending flags. Empty-result behavior and any
additional final XML signaling need runtime confirmation. Count-based EOF
must count protocol frames, including preroll/parameter-only records, before
producer filtering.

The SDK response branch at `0x2ae1d0..0x2ae1de` handles only subtype 2 XML and
subtype 3 media for live/stored packets. It ignores subtype 4; this investigation
has no evidence of an explicit 6/4 search-complete packet.

## Audio input and talkdown

The existing ihub contract accepts at most 30 seconds of mono signed 16-bit
little-endian PCM, requires an output with SDK format 0 at the exact requested
sample rate, uses `BufSizeSamples` for paced chunks and closes its dedicated
session after the final buffer. `EVAPI_SendAudio(handle,id,0,pcm,bytes)` does
not mean that PCM16 is sent unchanged on the network.

The SDK path is `EVAPI_SendAudio` (`0x36c600`) → `FSendAudio` (`0x2f7240`) →
`CAudioEncoder::Encode` (`0x3975a0`) → `FSendStreamPacket` (`0x2f5d80`). It:

1. Finds AudioIO by audio CASID and requires `fOutput` at AudioIO offset `0x20`.
2. For the uncompressed-input argument 0, requires even input byte length and
   encodes according to AudioIO.streamtype at offset `0x24`.
3. Sends to AudioIO.streamid at offset `0x38`, not the audio CASID.
4. Appends a 32-byte `tagStreamHeader` and the encoded bytes to outer type 5,
   subtype 3. The header contains client `gettimeofday` seconds and
   microseconds/1000, type 0, flags 0, motion length 0 and zero reserved bytes.

`AudioIO::UpdateStreamInfo` (`0x395280`) loads `AudioChannel@Standard`, converts
it to an internal byte by adding `0x90`, and reads `SampleRate`, `BitRate`,
`BufSizeSamples` and `ID`. `FGetAudioInfo` (`0x395120`) subtracts `0x90` again
when exposing the SDK format. Format 0 therefore maps to internal `0x90`,
whose encoder calls **G.711 mu-law**, at `0x397640`. The SDK's legacy name
"PCM" for that format must not be read as linear PCM wire encoding.

`Client.SendAudio` implements this format-0 contract, validates inventory and
rate, zero-pads the final PCM buffer before mu-law encoding, paces using a
monotonic clock, processes control/rekey packets concurrently and closes the
dedicated connection after allowing the final buffer duration. It rejects
other output formats. No extra Add/Drop, XML ownership transaction or output
acknowledgment was found in `FSendAudio`; successful completion means transport
submission, not proof of audible playback. Subsequent bounded hardware tests
detected a native test chirp through the speaker's internal microphone and
restored the changed settings; see [validation](validation.md). This is signal
detection evidence, not room-coverage or intelligibility validation.

The mu-law encoder is checked over all 65,536 signed 16-bit inputs against
the native instructions of `G711::ULawEncode(short)` at `0x3a30c0`, executed
in a local x86-64 emulator. The SHA-256 of encoded values in ascending signed
input order is
`90c29de505fb68e766118303bd552a16005dcf810873698bee1d8f3b247ce28c`.
This also verifies negative saturation and the SDK's one's-complement negative
sample convention. The test stores only this deterministic audio-codec digest.

Incoming audio uses the existing live Add request with an audio CASID (bit 7
set). `ProcessLiveAudioStream` (`0x303e80`) maps wire ID through configuration
and forwards media bytes unchanged to `CreateAudioEvent` (`0x2f5f70`).
`AudioEvent::PvGetBuffer` (`0x2f7570`) returns those bytes unchanged for the
compressed-buffer API, or decodes them using the configured audio format:

| Config/SDK format | Wire media bytes after the common stream header |
| --- | --- |
| 0 | Raw G.711 mu-law, one byte per sample |
| 1 | IMA ADPCM with a 32-byte header; predictor at 28 and index at 30; not implemented |
| 4 | AAC with single MPEG4-generic AU header: `00 10`, then big-endian 13-bit byte size and 3-bit index, then raw AAC AU |
| 5 | G.726; packing/bitrate variants require further verification; not implemented |
| 6 | Raw signed PCM16, host little endian in this Linux SDK |

The AAC validation is explicit in `AVCodecAudioDecoder::Decompress`
(`0x39ed29..0x39ed67`): check bytes 0 and 1, calculate `byte2<<5 | byte3>>3`,
validate that size+4 fits, and pass only those AU bytes to the decoder. The Go
`audioPayload` helper implements formats 0, 4 and 6 with bounds checks and
rejects other formats. Adding a go2rtc audio track still requires selecting the
audio input, negotiating its codec configuration and validating a real stream;
the existence of this helper is not evidence of producer audio coverage.

## Implemented parser validation

`pkg/exacq/media_packet.go` parses the independently established 32-byte media
header, retains motion/reserved bytes and raw flags, and distinguishes the six
known frame/media kinds. Its synthetic fixtures exercise byte offsets, zero
timestamps, motion boundaries, unknown types, every truncated header length,
and oversized motion lengths. A five-second fuzz run completed 395,249 inputs
without failure.

`pkg/exacq/media_requests.go` serializes live add/drop and single-camera stored
search XML. The search parser preserves recording gaps, signed stream mappings,
context/audio CASIDs and packet counts. Tests cover missing attributes,
malformed XML, trailing roots, overflows, invalid identifiers, media-specific
metadata, valid empty results and the recorder's nonstandard XML envelope.
Repeated short fuzz runs exceeded 200,000 total inputs without failure. These
are parser tests; hardware compatibility and performance results belong to the
runtime validation report.

Audio tests cover the complete mu-law input range, outgoing headers, final
padding, input/output validation, cancellation, server rejection and transport
rekey during paced audio. They pass the Go race detector. A five-second
`FuzzAudioPayload` run completed 413,912 inputs without failure.

## Boundaries still requiring proof

- Raw captured headers/media for both H264 and H265, flags, stream IDs and
  cached untimed frames.
- Wire PTS semantics for B frames, reserved-byte timing, and correct DTS
  derivation from the elementary stream if no wire DTS exists.
- Real incoming-audio codec/configuration compatibility, audible talkdown,
  unsupported audio formats, PTZ requests and relevant server permissions.
- Playback cancellation, negative/empty results, exact inclusive end behavior,
  snapshot search children and reconnect behavior.
- Motion bitmap layout and event/config XML semantics; motion payload is
  distinct from asynchronous alert notifications.
- Packet transforms and authenticated server negotiation (separate auth work).

Do not use this static notebook as evidence that any of these outstanding
features has passed a real camera test.
