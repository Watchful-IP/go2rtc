# Native HLS input

HTTP(S) HLS sources use MPEG-TS or fragmented MP4/CMAF directly, without spawning
FFmpeg. `EXT-X-MAP` selects the fMP4 path. H.264 (`avc1`/`avc3`), HEVC
(`hvc1`/`hev1`), and AAC-LC are supported. Non-audio/video metadata tracks are
ignored, including Verkada's `mecv` track without a media header.

Supported playlist features:

- Media playlists and the first variant of a master playlist; relative URLs are
  resolved against the final redirected URL. Query strings follow normal URL
  resolution, so segment-specific signed queries are retained, not invented.
- Media-sequence tracking, live reloads, ENDLIST, init-map changes, and byte ranges
  with explicit or valid implicit offsets. Range responses must match exactly.
- Identity AES-128 CBC with explicit or media-sequence IVs, PKCS#7 validation,
  encrypted initialization sections (explicit IV required), and rotating keys.
  Keys are fetched per resource, so rotation at an unchanged URI works.
- Compatible discontinuities rebase onto a continuous output timeline. Changed
  codecs/track configurations return an error for go2rtc's stream reconnect path.
- Downloads overlap paced playback with one prefetched segment. This matters for
  narrow live windows, such as Verkada low-resolution one-second segments.

All response bodies close on success or failure. Stop cancels downloads, polling,
retries, and pacing. HTTP timeouts use `core.ConnDialTimeout`; transport failures,
429s, and 5xx responses get at most two retries. Errors omit signed URLs. Custom
headers follow same-origin requests and are removed across origins. Limits are
2 MiB per playlist, 8 MiB per init, 64 MiB per segment, 32 tracks, and 65,536
samples per segment. A live gap, expired window, or prolonged stall returns an
explicit error rather than replaying old segments indefinitely.

## Deliberate limits

Use an explicit `ffmpeg:` source for SAMPLE-AES/DRM, separate audio/video
renditions, unsupported codecs, and composition offsets outside the native ingest range
(negative or more than 65,535 codec-clock ticks).
There is no automatic FFmpeg fallback. LL-HLS partial segments are not consumed;
playlists exposing complete segments can still play at ordinary HLS latency.
Standalone/progressive MP4 input is outside this change. Container support does
not add HEVC decoding to browsers that lack it.

## Output timing and reconnects

Explicit sample decode times, durations, and composition offsets are retained
through the raw packet pipeline. The MP4 muxer uses this timing instead of
inferring durations from presentation timestamps. This matters for B frames,
variable frame rates, and AAC encoder lead-in. Internal timing metadata is not
sent in RTP headers; payloaders preserve the presentation clock.

A new producer clock waits for a new video keyframe and continues the output
clock rather than rewinding it. Queued packets from the retired producer are
ignored. Audio arriving before the first video worker runs is retained in a
bounded lead-in (128 samples, at most 64 KiB, with owned payload storage).

The initial decoded-frame test missed a timing defect: H.264 frames spanning
1.0 second were remuxed across 1.6 seconds. The stronger test runs the actual HLS
producer and asynchronous MP4 consumer and compares all audio/video sample
counts, DTS, PTS, durations, and AAC payload hashes against ffprobe. It now passes
within one codec-clock tick. CI installs FFmpeg/ffprobe so these tests cannot
silently skip there. Fault tests also cover partial response cancellation,
exhausted HTTP retries, and a truncated source alongside a healthy source.

## Evidence and prior art

- [RFC 8216](https://www.rfc-editor.org/rfc/rfc8216.html), sections 3.3,
  4.3.2.2, 4.3.2.4, 4.3.2.5, 5, and 6.3, defines fMP4 initialization,
  ranges, encryption, sequence tracking, and reload behavior.
- [MediaMTX's mediacommon fMP4 parser](https://github.com/bluenviron/mediacommon/blob/08d7f843b1b4ded993ad8480297034d781b161f5/pkg/formats/fmp4/parts.go)
  is prior art for processing every fragment and track with bounded sample
  counts. Its offset handling assumes a moof-relative `trun` offset; this input
  also handles explicit/implicit bases, signed offsets, subsequent runs, and
  `trex` defaults. No dependency or source copy was added.
- [gohlslib's fMP4 processor](https://github.com/bluenviron/gohlslib/blob/bcc81c82ffb58d0c728561abf90cd70b23b16eda/client_stream_processor_fmp4.go)
  provides the segment-oriented producer model. Replacing all HLS input with
  that library would also require its codec/timing integration and Go 1.26 at
  the inspected revision; this fork remains on Go 1.25.
- [FFmpeg MOV demuxing](https://github.com/FFmpeg/FFmpeg/blob/master/libavformat/mov.c)
  and ffprobe are the independent packet/timing oracle, not this repository's
  own muxer. Tests compare every payload hash, DTS, PTS, and packet count.

The committed fixtures are synthetic test patterns and sine-wave audio generated
with FFmpeg 8.1.2. Each MP4 contains at least 11 fragments with shared audio/video
`mdat` data. FFmpeg round-trip tests additionally compare decoded video frame
hashes after go2rtc remuxing. See `../mp4/testdata/README.md` for regeneration.
Private Verkada captures stay outside Git. On 2026-09-16, all 436 packets in five
private H.264/HEVC + AAC segments matched ffprobe exactly. Separate 12-second
live checks passed for each resolution (288 video and 188 AAC packets each).
The low-resolution check exposed sequential-download lag and passed after bounded
prefetch was added. A subsequent combined rerun could not obtain a streaming URL
because the external credential helper timed out; it did not reach ingestion.

```sh
go test ./pkg/hls ./pkg/mp4 ./pkg/ivideon
go test -race ./pkg/hls ./pkg/mp4 ./pkg/ivideon
go test ./pkg/mp4 -run '^$' -fuzz '^FuzzProbe$' -fuzztime=30s
go test ./pkg/mp4 -run '^$' -fuzz '^FuzzDemux$' -fuzztime=30s
go test ./pkg/hls -run '^$' -fuzz '^FuzzPlaylist$' -fuzztime=30s
```

Optional external checks require ffprobe and an explicit local fixture directory
(`GO2RTC_HLS_FIXTURES`) or URL-generating script (`GO2RTC_HLS_LIVE_SCRIPT`). They
log packet counts only. Never commit customer footage or signed URLs.
