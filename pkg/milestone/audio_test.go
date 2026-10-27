package milestone

import (
	"encoding/binary"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// micLive is a microphone's live reply: one ADTS frame per audio packet from
// the synthetic tone, stamped from `from` in whole milliseconds as the recorder does.
func micLive(t *testing.T, from int64, n int) []byte {
	adts := fixture(t, "tone48k.aac")
	var out []byte
	for i := 0; i < n && aac.IsADTS(adts); i++ {
		size := int(aac.ReadADTSSize(adts))
		packet := make([]byte, audioHeaderSize+size)
		binary.BigEndian.PutUint16(packet, gbdAudioPacket)
		binary.BigEndian.PutUint32(packet[2:], uint32(len(packet)))
		binary.BigEndian.PutUint16(packet[6:], audioAACMPEG4)
		binary.BigEndian.PutUint16(packet[8:], uint16(i))
		at := from + int64(i)*1024*1000/48000
		binary.BigEndian.PutUint64(packet[12:], uint64(at))
		binary.BigEndian.PutUint32(packet[20:], 1024)
		packet[24], packet[25] = 1, 16
		binary.BigEndian.PutUint32(packet[26:], 48000)
		binary.BigEndian.PutUint16(packet[30:], 1)
		copy(packet[audioHeaderSize:], adts[:size])
		adts = adts[size:]

		out = append(out, "ImageResponse\r\nRequestId: 2\r\nPrev: 0\r\nCurrent: "+strconv.FormatInt(at, 10)+
			"\r\nNext: 0\r\nContent-length: "+strconv.Itoa(len(packet))+
			"\r\nContent-type: application/x-genericbytedata-octet-stream\r\n\r\n"...)
		out = append(out, packet...)
		out = append(out, "\r\n\r\n"...)
	}
	return out
}

// firstKeyframe is the recorder time of a live fixture's first keyframe.
func firstKeyframe(t *testing.T, name string) int64 {
	for _, f := range liveFrames(t, name) {
		if f.key {
			return f.time
		}
	}
	t.Fatal("no keyframe in " + name)
	return 0
}

type timedPacket struct {
	core.SampleTiming
	timed   bool
	payload []byte
}

// collectAV runs a live producer until want audio packets arrive, then stops it.
func collectAV(t *testing.T, prod core.Producer, want int) ([]timedPacket, []timedPacket) {
	medias := prod.GetMedias()
	require.Len(t, medias, 2)
	var mu sync.Mutex
	var video, audio []timedPacket
	got := make(chan struct{})
	record := func(dst *[]timedPacket, signal bool) func(*core.Packet) {
		return func(packet *core.Packet) {
			timing, ok := core.GetSampleTiming(packet)
			mu.Lock()
			defer mu.Unlock()
			*dst = append(*dst, timedPacket{timing, ok, packet.Payload})
			if signal && len(*dst) == want {
				close(got)
			}
		}
	}
	v, err := prod.GetTrack(medias[0], medias[0].Codecs[0])
	require.NoError(t, err)
	v.Input = record(&video, false)
	a, err := prod.GetTrack(medias[1], medias[1].Codecs[0])
	require.NoError(t, err)
	a.Input = record(&audio, true)

	done := make(chan struct{})
	go func() {
		_ = prod.Start()
		close(done)
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("audio did not arrive")
	}
	require.NoError(t, prod.Stop())
	<-done
	mu.Lock()
	defer mu.Unlock()
	return append([]timedPacket(nil), video[:]...), append([]timedPacket(nil), audio[:want]...)
}

func TestLiveMicrophoneJoinsVideoOnOneClock(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fx264_live.bin")
	rec.holdOpen = true
	base := firstKeyframe(t, "fx264_live.bin")
	rec.mic = micLive(t, base-512, 40) // the first 24 packets end before the video starts

	prod, err := Dial(rec.url("&mic=" + testMic))
	require.NoError(t, err)
	codec := prod.GetMedias()[1].Codecs[0]
	require.Equal(t, core.CodecAAC, codec.Name)
	require.Equal(t, uint32(48000), codec.ClockRate)

	video, audio := collectAV(t, prod, 16)
	require.NotEmpty(t, video)
	require.True(t, video[0].timed, "video carries explicit times when audio joins it")
	require.Zero(t, video[0].DecodeTime)

	for i, p := range audio {
		require.True(t, p.timed)
		require.Equal(t, video[0].ClockID, p.ClockID, "one clock for both tracks")
		require.False(t, aac.IsADTS(p.payload), "raw access units, ADTS header removed")
		if i > 0 {
			// Recorder times step 21 or 22 ms; overlapping samples would be dropped by the muxer.
			require.Equal(t, audio[i-1].DecodeTime+1024, p.DecodeTime, "packet %d", i)
		}
	}
	require.InDelta(t, 0, float64(audio[0].DecodeTime), 48*22, "audio starts with the video, not with the older buffered audio")
}

func TestLiveMicrophoneFailureKeepsVideo(t *testing.T) {
	rec := newFakeRecorder(t)
	rec.live = fixture(t, "fx264_live.bin")
	rec.micConnect = `<?xml version="1.0" encoding="UTF-8"?><methodresponse><requestid>1</requestid><methodname>connect</methodname><connected>no</connected><errorreason>Unknown device</errorreason></methodresponse>` + "\r\n\r\n"

	prod, err := Dial(rec.url("&mic=" + testMic))
	require.NoError(t, err)
	require.Len(t, prod.GetMedias(), 1)
	frames, _ := collect(t, prod)
	require.Len(t, frames, 25)
	require.False(t, frames[0].timed, "video without audio keeps recorder times")
}

func TestAudioCodec(t *testing.T) {
	for _, tc := range []struct {
		codec, subtype uint16
		want           string
	}{
		{audioG711, g711MuLaw, core.CodecPCMU},
		{audioG711, g711ALaw, core.CodecPCMA},
	} {
		codec, err := audioCodec(&audioPacket{codec: tc.codec, subtype: tc.subtype})
		require.NoError(t, err)
		require.Equal(t, tc.want, codec.Name)
		require.Equal(t, uint32(8000), codec.ClockRate)
	}
	_, err := audioCodec(&audioPacket{codec: 0x0009, subtype: 1}) // G.726
	require.Error(t, err)
}
