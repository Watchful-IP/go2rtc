package exacq

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
	"github.com/stretchr/testify/require"
)

// jpegMedia splits the synthetic MJPEG fixture into one recorder packet per
// JPEG, spaced 200 ms apart (recorders send JPEG well below the nominal rate).
func jpegMedia(t *testing.T, startMs int64) []*native.MediaPacket {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "frames.mjpeg"))
	require.NoError(t, err)
	var starts []int
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0xff && data[i+1] == 0xd8 && data[i+2] == 0xff {
			starts = append(starts, i)
		}
	}
	starts = append(starts, len(data))
	var packets []*native.MediaPacket
	for i := 0; i+1 < len(starts); i++ {
		stamp := startMs + int64(i)*200
		packets = append(packets, &native.MediaPacket{StreamID: 17, Type: native.MediaIFrame, Seconds: uint32(stamp / 1000), Milliseconds: uint16(stamp % 1000), Data: bytes.Clone(data[starts[i]:starts[i+1]])})
	}
	require.Len(t, packets, 6)
	return packets
}

func TestProducerLiveJPEGPassesFramesThrough(t *testing.T) {
	frames := jpegMedia(t, 1_700_000_000_000)
	p := mockProducer(t, mustParse(t, "exacq://user:password@recorder/101?select=lowest"), native.FormatJPEG, &fakeMediaSession{live: frames})
	require.NoError(t, p.probe(context.Background()))
	require.Equal(t, core.CodecJPEG, p.video.Codec.Name)
	require.Equal(t, frames[0].Timestamp(), p.ActualStart, "the first JPEG starts the stream: no keyframe wait")
	var payloads [][]byte
	var timings []core.SampleTiming
	p.video.Input = func(packet *core.Packet) {
		payloads = append(payloads, packet.Payload)
		timing, _ := core.GetSampleTiming(packet)
		timings = append(timings, timing)
	}
	require.ErrorIs(t, p.Start(), io.EOF, "a live session ending is a failure")
	require.Len(t, payloads, 5, "each JPEG is emitted whole; the last awaits its successor's timestamp")
	for i, payload := range payloads {
		require.Equal(t, frames[i].Data, payload)
	}
	for i := 1; i < len(timings); i++ {
		require.Equal(t, uint64(i)*18000, timings[i].DecodeTime, "recorder timestamps, 200 ms apart")
	}
}

func TestProducerPlaybackJPEGSearchesASecondAtATime(t *testing.T) {
	frames := jpegMedia(t, 1_700_000_000_000)
	source, err := ParseURL("exacq://user:password@recorder/101?start=1700000000000&end=1700000003000&speed=32")
	require.NoError(t, err)
	session := &fakeMediaSession{batches: [][]*native.MediaPacket{frames[:5], frames[5:]}}
	p := mockProducer(t, source, native.FormatJPEG, session)
	require.NoError(t, p.probe(context.Background()))
	require.Equal(t, core.CodecJPEG, p.video.Codec.Name)
	var output []*core.Packet
	p.video.Input = func(packet *core.Packet) { output = append(output, packet) }
	require.NoError(t, p.Start(), "a finite JPEG window ends cleanly")
	require.Len(t, output, 6)
	for _, r := range session.requests {
		require.LessOrEqual(t, r[1].Sub(r[0]), time.Second)
	}
	select {
	case <-p.video.Done():
	default:
		t.Fatal("finite receiver did not end")
	}
}
