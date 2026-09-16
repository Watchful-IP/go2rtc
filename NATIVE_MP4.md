# Native progressive MP4 input

HTTP progressive MP4 files now enter the existing go2rtc encoded-sample graph,
allowing the MP4 consumer to serve browser playback and recorder output without
an FFmpeg process. Eyevinn/mp4ff v0.56.0 supplies sample-table parsing; the existing
Watchful fMP4 demuxer supplies codec configuration and the existing muxer supplies
output fragments. No transcoding occurs.

## Input contract

- Finite, immutable HTTP files containing H.264, H.265, and/or AAC-LC.
- Recognized by MP4 Content-Type, `.mp4` extension, or `ftyp` when Content-Type is
  absent or application/octet-stream (including opaque proxy URLs).
- Movie index before or after media data, 32/64-bit chunk offsets, variable sample
  durations, positive composition offsets, identity edits, and empty lead-ins.
- Samples retain source DTS, duration, and CTS, converted by timestamp endpoints
  into codec clocks. A shared clock identifier preserves cross-track alignment.
- Byte-range requests preserve the final redirected request's authentication
  headers. Responses must match the requested range and known file validator.
- Without advertised byte ranges, a private temporary file is spooled and removed
  on failure, completion, or cancellation.

Bounds: 64 GiB ranged input, 256 MiB spool, 8 MiB movie index, 4,096 top-level
boxes, 1,000,000 samples, 16 MiB per sample, 24-hour track duration/lead-in;
15-second range requests and 30-second spool/index discovery timeouts.

Encryption, external sample data, trimming/rate-changing edits, negative CTS,
multiple sample descriptions, and direct fragmented MP4 input are rejected.
Existing native fMP4 HLS support remains the path for fragmented playlists.
This is a constrained ingestion contract, not a general MP4 editor or seek API.
Optional container metadata is not preserved by the existing output muxer.

## Finite lifecycle

Samples are paced for playback. Finite receivers apply bounded backpressure,
instead of the live graph's queue-overflow drops. Cancellation unblocks a stalled
sender. Producers are never automatically reconnected after completion or failure:
reopening a finite file would duplicate the recorded prefix.

Track completion drains MP4 consumer queues before ending output. HTTP MP4 ends
its body; MSE WebSocket sends `{"type":"end"}` after the final fragment. Mid-file
failures remain errors. The companion Watchful frontend change closes the socket,
drains pending SourceBuffer appends, ends MSE, and disables reconnect/watchdogs
while retaining the clip for replay and seeking. Deploy that frontend support
before enabling this go2rtc version for finite archive sources.

Other consumer protocols do not yet expose the finite EOF notification. A late
consumer attached to an already-completed producer receives EOF; replay requires
releasing that stream session and opening a new one. Archive seek/window selection
remains the responsibility of the caller and upstream provider.

## Evidence and validation

The staging LVT failure began with a 32-byte `ftyp` box (`00000020`), followed by
`free`, `mdat`, and a tail `moov`. It is a progressive H.264 MP4, not an unsupported
H.264 encoding. Its private 240-frame fixture is intentionally excluded from Git.

Synthetic fixtures cover H.264 B-frames/AAC, H.265/AAC, fast-start and tail indexes.
Packet payload hashes, DTS, PTS, and duration are checked against independent
ffprobe oracles. Tests also cover malformed tables, co64, range/auth behavior,
changed/truncated responses, spool cleanup/cancellation, finite EOF/no replay,
late consumers, and blocked-sender cancellation. A bounded metadata fuzz target is
included.

```sh
go test -race ./pkg/core ./pkg/mp4 ./internal/http ./internal/mp4 ./internal/streams
go test ./pkg/mp4 -run '^$' -fuzz FuzzProgressiveIndex -fuzztime=15s
GO2RTC_TEST_MP4=/private/clip.mp4 GO2RTC_TEST_MP4_OUTPUT=/tmp/remux.mp4 \
  go test ./pkg/mp4 -run TestLVTFile -count=1
```

The actual LVT fixture produced all 240 frames with identical decoded frame hashes.
A local built-server check verified HTTP output and Chrome playback through the
updated Fmp4Stream: one connection, 240 frames, 16.003666 seconds, one EOF, no error.
Focused race tests, fuzzing, and the Go build pass. The full repository test suite
also reports pre-existing ALSA/V4L2 platform, FFmpeg expectation, HomeKit, and Tuya
failures; the applicable non-platform failures reproduce on the base commit.

## Prior art

- [Eyevinn/mp4ff progressive-file segmenter](https://github.com/Eyevinn/mp4ff/tree/v0.56.0/examples/segmenter): sample-table extraction.
- [Eyevinn/mp4ff](https://github.com/Eyevinn/mp4ff/tree/v0.56.0/mp4): bounded box decoding and progressive metadata types.
- [MediaCommon progressive MP4 reader](https://github.com/bluenviron/mediacommon/tree/main/pkg/formats/pmp4): seekable finite-file ingestion as a separate input from fragmented streams.
