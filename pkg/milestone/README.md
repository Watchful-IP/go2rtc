# Milestone XProtect

Live and recorded video straight from an XProtect Recording Server over the
ImageServer protocol (TCP 7563), the protocol Smart Client uses. Video is passed
through as recorded: no Mobile Server, no extra camera streams, no transcoding.

```yaml
streams:
  door_live: milestonex://recorder:7563/<camera-guid>?token=<connection-token>
  door_live_audio: milestonex://recorder:7563/<camera-guid>?token=<connection-token>&mic=<microphone-guid>
  door_alarm: milestonex://recorder/<camera-guid>?token=<connection-token>&start=1790914879368&end=1790914889368
```

| Parameter | |
|---|---|
| `milestone://` | plain TCP |
| `milestones://` | TLS, certificate verified (skipped for IP hosts) |
| `milestonex://` | TLS, certificate not verified; recorder certificates name the Windows host |
| path | camera GUID |
| `token` | URL-encoded connection token from `ServerCommandService` `Login` |
| `stream` | stream GUID; the camera's default live stream when absent |
| `mic` | microphone GUID; its live audio joins the stream as a second track |
| `start`, `end` | recorder-clock time, unix milliseconds or RFC 3339; `start` selects playback |
| `speed` | playback rate, up to 32 |
| `jpeg=1` | ask the recorder for JPEG |

The source does not log in. The caller mints the token (OAuth `/IDP/connect/token`,
then `ServerCommandServiceOAuth.svc` `Login`) and starts a new session before
it expires (4 hours by default). Use the recorder's clock for `start` and `end`;
recorder clocks drift from real time.

## Live

The recorder answers `live` with its pre-buffered GOP, so the first keyframe
arrives immediately. Recorder times are used as presentation times.

B-frame streams arrive in decode order, stamped with arrival times that bunch
up per mini-GOP. They are reordered by picture order count (H.264 and H.265)
and spread evenly at the frame interval: the signalled H.264 frame rate, then the
measured keyframe-to-keyframe interval, which also re-anchors every GOP so live
latency cannot drift. The first pictures are held until the stream is known to
use B-frames or not (at most 300 ms, or 2 s for a B-frame stream with no
signalled rate).

The livepackage keepalive arrives at least every 5 seconds. A camera the recorder
reports as disconnected fails the session with a clear error instead of hanging.

### Audio

XProtect keeps a camera's audio on a separate microphone device (the camera's
related microphone). With `mic`, a second ImageServer connection reads it live,
passed through as recorded: AAC (ADTS stripped) or G.711. The microphone is dialled
while the video probes, and video never waits more than 2 seconds for it; a
microphone that fails or is offline leaves a video-only stream.

Both tracks then carry explicit times on one clock, so players line them up:

- Audio starts at the video's first keyframe. The microphone's shorter pre-buffer
  can make it start a few hundred milliseconds later.
- Recorder times step in whole milliseconds and an AAC frame is 21.33 ms, so audio
  advances by its sample count and re-anchors only when the two clocks part by
  more than 200 ms.
- Video without B-frames is held one picture so each picture's duration is known.

## Playback

The ImageServer answers `goto` and `next` with one GOP each. Requests are
pipelined (three GOPs, or twelve pictures for JPEG) and the pictures re-paced
here with explicit decode and presentation times, so playback runs at `speed`
without per-picture round trips. Output starts at the keyframe before `start`
and ends at `end` (finite: the stream is not reconnected).

- Gaps between motion recordings are closed rather than waited out.
- A `goto` into a gap answers with the last GOP before it; that footage is not
  played as the requested time. Newer footage is waited for briefly.
- At the newest recording, `next` repeats the last GOP. The database edge is
  polled every 500 ms for up to 10 seconds, so alarms seconds old play back.

## Degrading for awkward cameras

| Camera | Result |
|---|---|
| H.264, H.265 | passthrough |
| MJPEG | passthrough JPEG track |
| MPEG-4 Part 2, MxPEG, other codecs, legacy video blocks | recorder-side JPEG |
| privacy mask (`PrivacyMask` header) | recorder-side JPEG, mask rendered by the recorder |
| codec change mid-session | session error; a live reconnect probes again |

Recorder-side JPEG costs recording-server CPU and is only used when passthrough
cannot be shown correctly.

## Limits

- Playback carries no audio yet; `mic` is ignored there. XProtect records a
  microphone only when a rule records it (by default, only on bookmarks).
- Audio codecs other than AAC and G.711 (PCM, G.726) leave a video-only stream.
- Connection tokens are not renewed (`connectupdate`); sessions longer than the
  token lifetime end.
- Choosing the adaptive-playback secondary recording is untested.

## Testing

Fixtures in `testdata/` are ImageServer responses captured from an XProtect
Express+ 2025 R3 recording server (synthetic clock footage), replayed by a
fake recorder in the tests. `h265*.hevc` are x265 encodes for H.265 B-frames.
`tone48k.aac` is a synthetic 1 kHz tone (FFmpeg AAC), wrapped as microphone
packets by the tests.
