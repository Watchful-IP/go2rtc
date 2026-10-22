# Native go2rtc producer

The producer uses pure Go for TCP transport, password authentication, inventory,
live H.264/H.265 and bounded recording searches. It uses the same recorder
address and native port for control and media. The vendor SDK and shared
library are not runtime dependencies.

```yaml
streams:
  recorder_camera:
    - exacq://user:password@recorder:22609/459264
  recorder_substream:
    - exacq://user:password@recorder:22609/459264?stream=268894720
  smallest_stream:
    - exacq://user:password@recorder:22609/459264?select=lowest
  recording:
    - exacq://user:password@recorder:22609/459264?start=1790000000000&end=1790000010000&speed=1
```

Percent-encode reserved characters in usernames and passwords. Camera and
stream identifiers come from the authenticated inventory; the illustrative
substream ID above is not a universal channel number. Port 22609 is the
default. The source URL exposed by the producer redacts the password.

When a stream's name is itself an `exacq://`, `exacqsdk://`, or `exacqsdks://`
URL, stream listings return a process-local opaque alias instead. Aliases are
stable until deletion, distinct even for URLs that differ only in credentials,
and accepted for lookup, updates, deletion, and DOT selection. Raw URLs remain
valid internal/configuration identities; named streams and other schemes keep
their names. Aliases are not persistent configuration identifiers and change
on restart. Recorder/proxy credentials and SDK capability paths are absent from
both list keys and producer URL diagnostics.

`select=lowest` plays the camera's smallest enabled H.264/H.265 stream and is
exclusive with `stream`. A configured, enabled stream can still send nothing
(an unconnected substream): after 4 s of silence the producer tries the next
larger stream on a fresh session, and remembers silent streams per recorder
serial for 10 minutes so later views try them last.

Before requesting media the producer reads the licence on its own session.
Invalid, expired or unlicensed recorders fail with `stream_unavailable`
(phase `license`); an unknown camera with `camera_not_found`; no usable stream
with `stream_unavailable` or `unsupported_codec`; a camera that sends no video
with `stream_unavailable` (phase `live`). These typed errors are what go2rtc
reports for the stream. See [errors](errors.md).

`start` and `end` accept Unix milliseconds or RFC3339 timestamps. Both are
required for playback; a range must be positive and at most one hour. `end` is
exclusive. Playback speed must be positive and at most 32. Only
`gaps=preserve` is accepted. Extremely slow speeds that exceed the media clock
representation produce an explicit error.

## Live behaviour

The selected stream must be enabled and have inventory format 5 (H.264) or 6
(H.265). An explicit stream must belong to the requested camera. Session-local
wire stream IDs are taken from the inventory and are not assumed to equal the
camera CASID.

A live request is answered at once with the current GOP so far: its keyframe
and every following picture, all with zero timestamps, then timed pictures
continue the same GOP (exacqVision 26.1; the same idea as Milestone's
pre-buffered GOP). When picture order shows the buffer runs straight into the
first timed picture, the producer back-dates the buffer from that picture at
the nominal interval and starts output immediately. Otherwise (a gap, a
B-frame buffer, unreadable picture order) it keeps only the parameter sets and
starts at the next timed keyframe. On the lab recorder first output arrives in
75–134 ms instead of after the next keyframe (1.8–3.3 s); live snapshots use
the same buffer and take 86–392 ms. Parameter sets are included in codec
configuration and repeated on keyframes.
A codec or parameter-set change ends the producer so go2rtc can reconnect and
probe the new track.

I/P streams hold one picture until the next timestamp arrives. This adds one
frame of buffering while providing exact sample durations for variable frame
rates and integer-millisecond timestamps such as alternating 66/67 ms at
15 fps. Nominal 66.667 ms durations would overlap some samples and can cause
downstream MP4 writers to discard frames. RTP timestamps and `core.SampleTiming`
come from the recorder's actual timestamps.

### B-frames

B-frame streams need no configuration. Like `pkg/milestone`, the first pictures
are held until the stream is known to reorder or not (at most 300 ms, or until
the picture interval is known, at most 2 s). The recorder stamps pictures in
decode order (H.264 monotonic; HEVC may step backwards), so the shared
`pkg/reorder` package presents them by picture order count, spaced at the
measured interval (last GOP's span per picture, then the signalled rate, then
anchor spacing) and re-anchored to the recorder clock at every keyframe. DTS is
contiguous within a GOP and never moves backwards. Recorder timestamps keep
their exact cadence only for I/P streams; B-frame streams are evenly spaced.
Live sessions require keyframe timestamps to advance; B-frame streams are
exempt for other pictures.

## Recording behaviour

The producer fetches recording segments in ten-second windows. The client
bounds every search by packet count and total bytes. Each search can include
the preceding GOP; the producer selects the nearest timed keyframe at or before
the requested start and removes overlapping frames from later searches. If no
preceding keyframe exists, it starts at the earliest following keyframe within
the requested range. Parameter sets from discarded earlier GOPs are retained.
The shared `PlaybackStart` selector also drives control playback preparation,
so preparing a fractional keyframe time and seeking to it does not repeatedly
move the start back by another GOP. `actual_start` on the
producer reports the first output picture's recorder time. It can precede the
requested `start` because that keyframe is needed to decode the requested
interval. A short range is not silently advanced past its only GOP.

Pictures at or beyond the requested end are excluded. Repeated pictures from
adjacent searches are removed by content identity, because B-frame timestamps
need not increase in decode order. Pacing uses absolute
recorder timestamp differences from `actual_start`, divided by the requested
speed. Gaps remain on the output timeline, and the preceding picture's duration
spans the gap. The output clock scales each boundary before subtraction to
avoid sample overlaps caused by fractional-speed rounding. The final sample
does not extend beyond the requested end.

Playback receivers apply backpressure and mark finite EOF after their final
picture. They do not replay automatically after completion or failure. Stop
interrupts network reads and pacing waits and releases blocked downstream
queues. A malformed or incomplete search is an error, not clean EOF.

## Current limits and evidence

- The media header has one timestamp and no separately verified PTS/DTS pair.
  B-frame presentation times are therefore reconstructed, evenly spaced, not
  recovered per picture.
- The direct producer exposes a video track. Audio, talkdown, PTZ and motion or
  alert configuration have separate platform contracts and are not implied by
  successful video streaming. See `platform-contracts.md` and the wire research.
- Snapshots use the separate native control adapter and decoder described in
  the protocol documentation; the streaming producer itself exposes video.
- JPEG and other video formats return an explicit unsupported-format error.
- Live output ends when a keyframe timestamp, or any I/P-stream timestamp,
  stops advancing. Authentication and transport failures are reported to go2rtc for normal live
  reconnection; finite recordings retain their failure result.

Synthetic H.264 and HEVC fixtures test cached parameter retention, keyframe
selection, exact alternating frame durations, preroll, segment overlap,
recording gaps, speed scaling, end filtering, EOF, cancellation, live start
from the buffered GOP and its fallback. Synthetic H.264 and HEVC B-frame
fixtures verify display-order presentation (live and playback) even when the
native header labels B pictures as P, and that snapshots decode the picture
FFmpeg shows at the selected display rank. Video fixtures contain a generated test
pattern, not camera footage. Hardware integration and performance observations
are recorded in `producer-validation.md`.
