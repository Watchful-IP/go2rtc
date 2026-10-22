# B-picture timing evidence

This records what the recorder does with B-frames. The producer now handles
them automatically with Milestone's POC reordering (`pkg/reorder`; see
[producer.md](producer.md#b-frames)). An earlier explicit fixed-GOP mode,
`reorder=gop&gop=N`, which recovered exact per-picture PTS from the timestamp
multiset, was retired: it needed the encoder's GOP length, which operators do
not know.

Recorder experiments with generated H.264 and HEVC established that a complete
GOP retains the source's presentation timestamp **multiset**, although timestamps
are not reliably attached to the corresponding decode-order picture. Codec POC
recovers the assignment. PTS is recovered from that evidence; DTS is synthesized
to satisfy decode order and is not an independently recovered wire field.

## Static and offline native evidence

The pinned Linux x64 `libevAPI.so` 1.47.27.1 has SHA256
`14bd776c0fb0c46b72f2d2034a04ea2fa6a5794d0876facb1fc20cf4bda13509`.

- `Payload::Payload` (`0x2f5ee0`) and `CPacket::CPacket` (`0x2f5e60`)
  copy the wire seconds/milliseconds into one timestamp. The callback's extra
  storage is not a verified independent DTS/PTS pair.
- `IsBFrame(int, unsigned char*, unsigned int)` (`0x1a7120`) accepts only
  internal codec `0x85`, calls `H264Parser::Parse`, and tests slice type `1`.
  A standalone native-library call, without a session or networking, matched
  FFmpeg on all 45 H.264 fixture pictures, including all 27 B-pictures.
  Access units were normalized to four-byte Annex B start codes, as observed
  on the wire; the helper did not classify the original mixed-length x264
  start codes correctly.
  The function returns false for internal HEVC codec `0x86`, including all 30
  actual B-pictures in the HEVC fixture. Its false return cannot prove that an
  HEVC stream has no reordering.
- `PSConvertAVCodecStream::WritePacket` (`0x1b9410`) obtains the stored timestamp
  with `CPSFile::GetTimestampFromPacket` at `0x1b95d7`; it is used for gaps.
  `DoWritePacket` (`0x1acd10`) derives output time from accumulated sample
  durations and writes the same value into `AVPacket.dts` at `0x1ace00` and
  `AVPacket.pts` at `0x1ace04`. DWARF establishes offsets 16 and 8 respectively.
  This export path is therefore unsuitable as an independent PTS/DTS oracle.
- The client `libdecompressor.so` has no DWARF, but retains function symbols.
  `DecoderSource::QueueCompressedData` (`0x22e80`) detects header frame type 3
  at `0x22f03`. `GetNextFrameTime` (`0x22930`) directly combines the queued
  header's seconds/milliseconds. `DecodeData` (`0x233f0`) delegates elementary
  stream reordering to the decoder. This establishes recognition of B-pictures,
  not a transport timestamp contract.

The standalone classification check is reproducible with
[`b-picture-oracle.py`](b-picture-oracle.py) on Linux x64:

```sh
uv run python docs/exacq/b-picture-oracle.py \
  --library /usr/local/exacq/webservice/bin/libevAPI.so \
  --fixtures pkg/exacq/testdata
```

## Generated codec tests

The 160x90, 15 fps `testsrc2` fixtures contain no camera footage. The decoder's
`pkt_pos` output independently maps compressed access units to display order.
Each of three closed GOPs produces these display indices in decode order:

| Codec | Display indices in decode order | POC scale |
| --- | --- | --- |
| H.264, two B-pictures | 0,3,1,2,6,4,5,9,7,8,12,10,11,14,13 | 2 |
| HEVC, three B-pictures with hierarchy | 0,4,2,1,3,8,6,5,7,12,10,9,11,14,13 | 1 |

`TestPictureOrderMatchesDecodedBFixtures` checks every POC, B classification,
and IDR reset for 90 pictures. Additional tests check unsupported field-picture
B detection and malformed SPS widths that would otherwise truncate through an
8-bit cast. The B guard now reads H.264 slice type before SPS-dependent fields;
unsupported field/POC formats cannot disable that check.

`FuzzPictureParser` completed 514,859 executions in a 10-second, two-worker run
without a failure. This is bounded parser evidence, not exhaustive fuzzing.

## Recorder and SDK input verification

A temporary RTSP fixture server ran on recorder loopback, with separate H.264
and HEVC paths. Each access unit logged its
decode index, display index, 90 kHz RTP presentation timestamp, send wall clock,
slice type, and VCL SHA256. The three-GOP fixtures repeat with continuous RTP
time. A watchdog bounded the temporary process. Registration used the ordinary
RTSP device plugin after a coordinated in-place speaker disable; no license
or MAC fields were changed. The existing camera IDs and recording schedules
were guarded by private snapshots and fresh readback. After the experiment,
the temporary device was removed and the speaker was restored in place. Three
fresh native checks and a fresh SDK baseline comparison confirmed restoration
of the original IDs, schedules, formats and enabled states.
The temporary source, watchdog and test-owned tunnel were stopped afterward;
both loopback listeners were verified closed.

An independent FFmpeg RTSP decode, using `-copyts -fps_mode passthrough`, matched
all 45 display-frame hashes for each codec against decoding the original files.
FFmpeg's default start-time clipping discarded some initial frames; that default
must not be mistaken for a missing B-picture in the input service.

The variable-timing source subtracts 2,400 ticks from every odd display index
on a 90 kHz, nominal 15 fps timeline. Its presentation intervals therefore
alternate between 40 ms and 93⅓ ms. Native live and archive captures preserved
this variation. Six complete GOPs per codec in each native path matched the
source timestamp multiset within 0.667 ms, consistent with integer-millisecond
recorder precision. H.264 timestamps were monotonic in decode order; HEVC
included backward steps. Neither behavior alone identifies per-picture PTS.

Separate, bounded captures from the actual SDK produced 66 H.264 and 65 HEVC
callbacks. Every full NAL sequence and picture kind matched a native capture
counterpart. Three complete GOPs per codec matched the same variable-timing
multiset within 0.667 ms. These were different wall-clock windows of the same
repeating fixture, not a claim of equal absolute timestamps across sessions.
See [SDK input equivalence](sdk-input-equivalence.md). The native runtime does
not load the SDK used by this separate oracle.

`TestLabBPictureCorrelation`, gated by `EXACQ_TEST_B_CAMERA` and
`EXACQ_TEST_B_OUT`, retains private live/archive observations. Generated,
sanitized timing fixtures lived under the retired `pkg/exacqtiming/testdata`
(see git history); physical camera footage and recorder credentials were never
committed.
