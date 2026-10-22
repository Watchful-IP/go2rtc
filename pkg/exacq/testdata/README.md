# Synthetic video fixtures

FFmpeg's generated `testsrc2` pattern (160x90, 15 fps), not camera footage.
Each access unit starts with an AUD.

- `ip.h264`, `ip.h265`: 30 frames, keyframe every 15, no B-frames.
- `b.h264`: 45 frames, keyframe every 15, 2 B-frames, no pyramid.
- `b.h265`: 24 frames, keyframe every 12, 3 B-frames.
- `b-closed.h265`: 45 frames, keyframe every 15, 3 B-frames, closed GOPs.
