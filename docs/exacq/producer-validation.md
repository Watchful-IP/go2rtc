# Native producer validation

These observations use the pure Go native protocol and direct go2rtc producer,
with the lab Linux recorder reached through one forwarded native TCP port.
The Windows host's separate original recorder was not used. Camera additions
and recording changes were coordinated by the parent task. Credentials, raw
configurations and camera footage remain outside the repository.

## Mixed-camera live and finite playback

Each source was tested sequentially for approximately six seconds of live MP4.
Finite recordings requested the same ten-second source interval at speed 2.
Archive output includes its required preceding keyframe. All six MP4s decoded
completely, with zero FFmpeg decoder errors, non-monotonic DTS values or sample
overlaps. None contained B pictures or nonzero composition offsets.

| Source | Codec and resolution | Live frames / duration | Live MP4 bytes | Archive frames / duration | Archive MP4 bytes |
|---|---|---:|---:|---:|---:|
| Axis Q1728 | H.264 1920×1080 | 90 / 6.000 s | 231,915 | 203 / 6.7645 s | 523,786 |
| Axis P1468 | HEVC 3840×2160 | 90 / 6.000 s | 1,289,577 | 157 / 5.2200 s | 2,090,062 |
| Dahua | HEVC 3840×2160 | 139 / 6.043 s | 5,409,674 | 248 / 5.3855 s | 9,332,313 |

Live timestamps measured 66/67 ms for Q1728, 40/80 ms for P1468, and 43/44 ms
for Dahua. P1468's actual 15 fps cadence differs from its codec's nominal
25 fps base, demonstrating why durations must follow recorder timestamps.
Archive responses reached finite EOF without a client-imposed duration.

The live HTTP time to first byte was 3.388 s, 3.072 s, and 0.240 s respectively;
the archive values were 43 ms, 63 ms, and 220 ms. Live startup includes waiting
for a timed keyframe, so it is not steady-state transport latency.

## Timestamp age and throughput

Separate three-second direct-producer measurements included one-frame duration
buffering and used the recorder timestamp against the receiving host clock.

| Source | Emitted fps | Encoded payload rate | Timestamp age median / p95 |
|---|---:|---:|---:|
| Q1728 | 14.98 | 0.309 Mbit/s | 78.4 / 79.2 ms |
| P1468 | 14.99 | 1.809 Mbit/s | 256.7 / 299.2 ms |
| Dahua | 23.32 | 7.247 Mbit/s | Clock skew prevents interpretation |

Dahua's source timestamps were ahead of the receiving host in this test. These
are timestamp-age observations, not glass-to-glass latency measurements. The
short samples are not capacity limits or long-term latency guarantees. The
separate coordinated soak evidence is maintained by the root validation task.

## Preserved recording gap and repeatable seeking

A controlled recording gap was retained in the Q1728 archive. The request
`[1792543920000, 1792543970000)` at speed 4 produced 526 pictures and a complete
1,406,837-byte MP4. It starts at the preceding keyframe 1792543918432 and has
duration 12.892 s. The maximum sample duration is 4.15 s, preserving the
16.600 s source gap. HTTP first byte took 74.7 ms and complete EOF took 12.960 s.
The final file has no overlapping samples or non-monotonic DTS values and
decodes without errors.

Decode validation uses `-fps_mode passthrough -enc_time_base 1:90000` when
writing FFmpeg's null output. Its default coarse output time base can produce
muxer timestamp warnings for accelerated irregular recordings even when the
input MP4 timestamps are strictly ordered; packet inspection and a preserved
output clock distinguish those warnings from decoder errors.

Control `playback.prepare` and the producer use the same nearest-keyframe
selection. Hardware preparation returned:

| Requested start (Unix ms) | Reported keyframe | Prepare again at reported time |
|---:|---:|---:|
| 1792543920000 | 1792543918432 | 1792543918432 |
| 1792543938000, inside gap | 1792543947365 | 1792543947365 |
| 1792543942000, inside gap | 1792543947365 | 1792543947365 |

The two requests inside the gap advance to its first available keyframe.
Repeating the fractional prepared time does not move backward by another GOP.
The gap test also exposed and fixed an additional 32-byte stored-stream marker
after the advertised media count; it is skipped only when its entire observed
shape matches and never substitutes for the count-based EOF contract.

## Fixed-GOP B-picture validation

> Historical record. The explicit fixed-GOP mode described here was retired in
> favour of automatic POC reordering shared with Milestone; see
> [producer.md](producer.md#b-frames).

A coordinated temporary RTSP input supplied generated 160×90 H.264 and HEVC
with fixed 15-picture closed GOPs. The tests used `reorder=gop&gop=15`; they do
not change the default I/P mode or establish general encoder compatibility.
The source's variable presentation intervals alternate between 40 ms and
93⅓ ms, independently of its nominal 15 fps decode-order emission.

Native live and archive inputs for both codecs matched the known VCL bytes,
POC and timestamp multiset. Separate captures from the actual SDK matched all
66 H.264 and 65 HEVC callback NAL sequences and picture kinds to native
counterparts. Three SDK GOPs, six native-live GOPs and six native-archive GOPs
per codec matched the source variable-timing multiset within 0.667 ms. See
[SDK input equivalence](sdk-input-equivalence.md) for the method and the
different-wall-clock-window qualification.

Final live producer runs for both codecs emitted and decoded 90/90 pictures.
Finite speed-4 archive runs emitted and decoded 45/45 pictures each and reached
normal EOF. Every decoded pixel hash matched the known 45-picture source cycle
in display order, allowing its independently determined starting rotation.
All four files decoded without errors. Live presentation deltas were
40/93/94 ms, preserving the source variation.

| Codec | Live pictures / positive CTS | Live probe / total elapsed | Archive pictures / positive CTS | Archive probe / complete elapsed | Actual source span / end extension |
| --- | ---: | ---: | ---: | ---: | ---: |
| H.264 | 90 / 30 | 1,417.2 / 6,418 ms | 45 / 15 | 37.8 / 763 ms | 3,026 / 589 ms |
| HEVC | 90 / 42 | 1,553.5 / 6,556 ms | 45 / 21 | 85.1 / 811 ms | 3,027 / 590 ms |

Live elapsed time includes probing followed by a bounded five-second read;
startup includes GOP closure. These are helper elapsed measurements rather
than glass-to-glass latency. Archive end extensions are the explicit whole-GOP
contract, not strict-end results. Two independent H.264 hardware searches
confirmed that reusing fractional prepared start/end values leaves the bounds
stable.

Historical snapshots requested one millisecond after the initial picture and
selected the independently decoded display picture 93 ms after that initial
picture, for both codecs. The 80×46 JPEG pixels matched the corresponding MP4
display picture exactly. Historical completion took 77 ms for H.264 and 158 ms
for HEVC; live JPEG completion took 1,223 ms and 1,866 ms respectively.

The shared helper requires an independently configured picture count, stable
parameters, progressive single-VCL syntax, dense unique POC and unique
timestamps. Tests cover the highest-POC-loss counterexample, malformed extra
slices, closed-IDR boundaries, inter-GOP gaps, data ownership, queued
cancellation, adjacent-search overlap and complete-GOP end reporting. PTS is
recovered from the qualified input multiset; DTS is synthesized, not another
observed wire timestamp. Unsealed motion cuts and EOF return errors.

After the experiment, the temporary device was deleted. Three fresh native
readbacks confirmed the original speaker enabled with its output preserved.
A fresh SDK inspection matched the private baseline fingerprints, original
device/camera IDs, recording schedules and formats: four enabled devices and
capacity four. No existing device was deleted or re-created and no license
or MAC value changed.
The temporary RTSP process, its watchdog and the test-owned tunnel were stopped,
with both loopback listeners verified closed. The later internal-microphone
speaker-output check and exact settings restoration are documented separately
in [validation](validation.md).

## Automated coverage and remaining limits

Synthetic H.264 and HEVC fixtures cover untimed cached frames, parameter-set
retention, nearest preroll, fractional repeat seeks, empty recording windows,
overlapping search segments, gap preservation, exact durations, speed scaling,
finite EOF and cancellation. Malformed payload tests and video fuzzing exercise
the untrusted bitstream boundary. A ten-second video fuzz run completed 55,054
executions without failure. Final race-enabled tests passed for `pkg/exacq`,
`internal/exacq`, `pkg/exacqtiming`, `pkg/core` and `pkg/mp4`. Complete
CGO-free Linux amd64 and arm64 binary builds also passed.

B pictures remain rejected by default using native classification and codec
slice parsing. The opt-in above supports only the verified fixed-GOP subset;
adaptive/open/multislice/field streams and unknown GOP counts are unsupported.
`FuzzTimingBuffer` completed 650,765 executions in a bounded ten-second run
without failure. The earlier physical-camera I/P tests and the generated
B-picture experiment are separate evidence. The producer exposes video;
audio-output and control evidence is documented separately.
