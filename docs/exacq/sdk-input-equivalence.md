# SDK and native media input equivalence

On 2026-10-21, bounded live captures from the actual Linux SDK and the pure Go
protocol client received the same synthetic H.264 and HEVC access units from
the lab recorder. Both preserved the source's variable frame timing as a
timestamp multiset within each complete GOP. This verifies the inputs available
to the two producers; it is not a claim about their final output timing.

The captures used temporary camera `3146240`, registered through the recorder's
ordinary RTSP input plugin. The source was reachable only at loopback port
`18554`, with `/h264` and `/h265` selected in separate test windows. The parent
thread controlled registration, codec switches, recording configuration and
cleanup. These SDK capture calls changed no device or recording settings.

## Actual SDK capture

The oracle loaded the pinned `libevAPI.so` documented in
[the authentication research](auth-research.md). It used the established ABI
from the SDK-backed provider's `native-media.ts`: callback payload minimum
72 bytes, camera ID at offset 0, signed 64-bit seconds at offset 8, unsigned
16-bit milliseconds at offset 16, and picture kind at offset 18.
`EVAPI_GetCompressedFrame` bytes were copied before the callback returned.
No C callback layout was substituted for the native wire header.

| Observation | H.264 | HEVC |
| --- | ---: | ---: |
| Capture duration | 4.218 s | 4.215 s |
| Compressed callbacks | 66 | 65 |
| Timestamped callbacks | 64 | 64 |
| I / P / B picture kinds | 5 / 23 / 38 | 5 / 18 / 42 |
| Compressed bytes | 60,135 | 69,814 |
| Unique VCL SHA-256 values | 45 | 45 |
| Backward timestamp steps among timed callbacks | 0 | 12 |
| Callback errors | 0 | 0 |

Both `EVAPI_StreamDelCamera` and its subsequent `EVAPI_Send` returned zero.
Each capture closed and cleaned up its session. The SDK gateway was not
restarted. The initial cached frames are included in callback totals; they
are excluded from complete-GOP timestamp checks.

## Access-unit and picture-kind comparison

Each codec fixture contains 45 access units and three 15-frame GOPs. VCL hashes
use the concatenated VCL NAL-unit bytes, excluding start codes. For the stronger
full-access-unit comparison, Annex-B start codes are replaced with a four-byte
big-endian NAL length before hashing every NAL in order.

All 66 H.264 SDK callbacks and all 65 HEVC SDK callbacks had a counterpart in
the corresponding 105-frame native live capture with identical complete NAL
sequences and identical I/P/B picture kinds. Every VCL hash belonged to the
source fixture. Both implementations covered all 45 source access units for
each codec. The observations were made in different wall-clock windows, so this
comparison deliberately matches repeating fixture content rather than asserting
equal absolute timestamps between sessions.

## Variable frame timing

The synthetic source emits access units in decode order. Its presentation
timestamp for display index `d` is:

```text
RTP ticks = (d + 3) * 6000 - (2400 if d is odd else 0)
clock rate = 90000 Hz
```

Thus presentation intervals alternate between 40 ms and 93⅓ ms. A constant
15 fps reconstruction would lose this variation.

For every complete, contiguous 15-frame GOP, the observed timestamps were sorted,
normalized to the first timestamp and compared with that source timeline.
The fixture's 45-frame cycle changes parity on alternate repetitions, so both
known cycle parities were considered. Access-unit hashes establish the complete
GOP membership independently of timestamp order.

| Complete GOPs checked per codec | H.264 | HEVC |
| --- | ---: | ---: |
| Actual SDK live callbacks | 3 | 3 |
| Native live packets | 6 | 6 |
| Native archive packets | 6 | 6 |
| Maximum normalized timestamp error | 0.667 ms | 0.667 ms |

All checked GOP timestamp multisets match the source variation within the
recorder's integer-millisecond precision. H.264's monotonic callback timestamps
and HEVC's backward steps demonstrate that the single timestamp cannot simply
be labeled as the presentation timestamp attached to each decode-order access
unit. The producer's picture-order reconstruction must be evaluated separately.

## Private retained evidence

The disposable capture helper is `/tmp/exacq-wire/sdk_capture.py`; it verifies
the saved temporary device ID and selected codec before requesting media.
Raw captures, mode `0600`, are `/tmp/exacq-wire/sdk-b-h264.json` and
`sdk-b-h265.json`. They contain copied Annex-B bytes, callback timestamps,
picture kinds, reception times and VCL hashes. Sanitized scalar comparisons are
`sdk-input-equivalence-h264.json` and `sdk-input-equivalence-h265.json` in the
same private directory. Native comparison captures are under
`capture-h264-vfr/` and `capture-h265-vfr/`, with separate `live.json` and
`archive.json` files.

No recorder or device credentials appear in these findings. General hardware
coverage remains separate from this controlled, repeating synthetic source.
