# Synthetic fMP4 fixtures

Generated with FFmpeg 8.1.2. The video is `testsrc2`; audio is a 440 Hz sine wave.
No customer footage, credentials, or downloaded media is included. The JSON
packet oracles were generated independently by ffprobe. Tests do not need
FFmpeg except the optional decoded-frame round-trip check.

Run from the repository root:

```sh
ffmpeg -y -f lavfi -i testsrc2=size=128x96:rate=10 \
  -f lavfi -i sine=frequency=440:sample_rate=48000 -t 1.1 \
  -c:v libx264 -preset ultrafast -g 10 -bf 2 -c:a aac -b:a 32k \
  -movflags empty_moov+default_base_moof -frag_duration 100000 \
  pkg/mp4/testdata/h264.mp4
ffmpeg -y -f lavfi -i testsrc2=size=128x96:rate=10 \
  -f lavfi -i sine=frequency=440:sample_rate=48000 -t 1.1 \
  -c:v libx265 -preset ultrafast -x265-params log-level=error:pools=1 \
  -tag:v hvc1 -g 10 -bf 0 -c:a aac -b:a 32k \
  -movflags empty_moov+default_base_moof -frag_duration 100000 \
  pkg/mp4/testdata/h265.mp4
for codec in h264 h265; do
  ffprobe -v error -show_packets -show_streams -show_data_hash sha256 \
    -show_entries packet=stream_index,dts,pts,duration,size,data_hash:stream=index,id,time_base,codec_name \
    -of json=compact=1 "pkg/mp4/testdata/$codec.mp4" > "pkg/mp4/testdata/$codec.json"
done
ffmpeg -y -i pkg/mp4/testdata/h264.mp4 -c copy -f mpegts pkg/mp4/testdata/h264.ts
```

The H.264 fixture includes B frames with positive composition offsets. Hand-built
unit fixtures cover signed data offsets, explicit and implicit bases, multiple
runs, inherited defaults, extended box sizes, malformed data, and long timelines.
