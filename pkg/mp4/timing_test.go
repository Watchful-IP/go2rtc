package mp4

import (
	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMuxerKeepsExplicitDurationsAndTrackOffsets(t *testing.T) {
	m := &Muxer{}
	for i := 0; i < 2; i++ {
		m.AddTrack(&core.Codec{Name: core.CodecAAC, ClockRate: 48000, Channels: 1, FmtpLine: "config=1188"})
	}
	init, err := m.GetInit()
	require.NoError(t, err)
	var payload []byte
	for _, s := range []struct {
		id       byte
		dts      uint64
		duration uint32
	}{{0, 1 << 40, 9600}, {1, 1<<40 + 480, 1024}, {0, 1<<40 + 9600, 512}} {
		p := &rtp.Packet{Payload: []byte{1, 2, 3}}
		core.SetSampleTiming(p, core.SampleTiming{DecodeTime: s.dts, Duration: s.duration})
		payload = append(payload, m.GetPayload(s.id, p)...)
	}
	d := &Demuxer{}
	_, err = d.Probe(init)
	require.NoError(t, err)
	samples, err := d.Demux(payload)
	require.NoError(t, err)
	require.Len(t, samples, 3)
	require.Equal(t, uint64(0), samples[0].DecodeTime)
	require.Equal(t, uint32(9600), samples[0].Duration)
	require.Equal(t, uint64(480), samples[1].DecodeTime)
	require.Equal(t, uint64(9600), samples[2].DecodeTime)
	require.Equal(t, uint32(512), samples[2].Duration)
	m.Reset()
	p := &rtp.Packet{Payload: []byte{1, 2, 3}}
	core.SetSampleTiming(p, core.SampleTiming{DecodeTime: 100, Duration: 1024})
	samples, err = d.Demux(m.GetPayload(0, p))
	require.NoError(t, err)
	require.Equal(t, uint64(0), samples[0].DecodeTime)
}

func TestMuxerReconnectPreservesTimeline(t *testing.T) {
	m := &Muxer{}
	m.AddTrack(&core.Codec{Name: core.CodecAAC, ClockRate: 48000, Channels: 1, FmtpLine: "config=1188"})
	init, err := m.GetInit()
	require.NoError(t, err)
	d := &Demuxer{}
	_, err = d.Probe(init)
	require.NoError(t, err)
	sample := func(id uint32, ts uint64) []byte {
		p := &rtp.Packet{Payload: []byte{1, 2}}
		core.SetSampleTiming(p, core.SampleTiming{ClockID: id, DecodeTime: ts, Duration: 1024})
		return m.GetPayload(0, p)
	}
	out := sample(1, 90000)
	out = append(out, sample(1, 91024)...)
	out = append(out, sample(2, 0)...)
	require.Empty(t, sample(1, 92048), "late packet from retired source")
	out = append(out, sample(2, 1024)...)
	samples, err := d.Demux(out)
	require.NoError(t, err)
	require.Len(t, samples, 4)
	for i, s := range samples {
		require.Equal(t, uint64(i*1024), s.DecodeTime)
	}
}

func TestInternalTimingDoesNotEscapeRTPPayloaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		pay     func(core.HandlerFunc) core.HandlerFunc
	}{
		{"h264", []byte{0, 0, 0, 2, 0x65, 0x80}, func(h core.HandlerFunc) core.HandlerFunc { return h264.RTPPay(1200, h) }},
		{"h265", []byte{0, 0, 0, 2, 0x26, 0x01}, func(h core.HandlerFunc) core.HandlerFunc { return h265.RTPPay(1200, h) }},
		{"aac", []byte{1, 2}, aac.RTPPay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			handler := tc.pay(func(p *core.Packet) {
				count++
				require.Equal(t, uint8(2), p.Version)
				require.Equal(t, uint32(43210), p.Timestamp)
				require.False(t, p.Extension)
				_, ok := core.GetSampleTiming(p)
				require.False(t, ok)
			})
			p := &core.Packet{Payload: tc.payload}
			p.Timestamp = 43210
			core.SetSampleTiming(p, core.SampleTiming{ClockID: 1, DecodeTime: 40000, Duration: 3000, CompositionOffset: 3210})
			handler(p)
			require.Positive(t, count)
		})
	}
}
