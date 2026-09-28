package mp4

import (
	"encoding/hex"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/iso"
	"github.com/pion/rtp"
)

type Muxer struct {
	index       uint32
	dts         []uint64
	pts         []uint32
	codecs      []*core.Codec
	timingEpoch uint64
	timingClock uint32
	timingID    uint32
	timingBase  uint64
	jpeg        []jpegClock
	now         func() time.Duration // monotonic; nil uses the process clock
}

type jpegClock struct {
	seen     bool
	interval uint32
	at       time.Duration
}

var processStart = time.Now()

func (m *Muxer) AddTrack(codec *core.Codec) {
	m.dts = append(m.dts, 0)
	m.pts = append(m.pts, 0)
	m.codecs = append(m.codecs, codec)
	m.jpeg = append(m.jpeg, jpegClock{})
}

func (m *Muxer) GetInit() ([]byte, error) {
	mv := iso.NewMovie(1024)
	mv.WriteFileType()

	mv.StartAtom(iso.Moov)
	mv.WriteMovieHeader()

	for i, codec := range m.codecs {
		switch codec.Name {
		case core.CodecH264:
			sps, pps := h264.GetParameterSet(codec.FmtpLine)
			// some dummy SPS and PPS not a problem for MP4, but problem for HLS :(
			if len(sps) == 0 {
				sps = []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
			}
			if len(pps) == 0 {
				pps = []byte{0x68, 0xce, 0x38, 0x80}
			}

			var width, height uint16
			if s := h264.DecodeSPS(sps); s != nil {
				width = s.Width()
				height = s.Height()
			} else {
				width = 1920
				height = 1080
			}

			mv.WriteVideoTrack(
				uint32(i+1), codec.Name, codec.ClockRate, width, height, h264.EncodeConfig(sps, pps),
			)

		case core.CodecH265:
			vps, sps, pps := h265.GetParameterSet(codec.FmtpLine)
			// some dummy SPS and PPS not a problem
			if len(vps) == 0 {
				vps = []byte{0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x01, 0x40, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xac, 0x09}
			}
			if len(sps) == 0 {
				sps = []byte{0x42, 0x01, 0x01, 0x01, 0x40, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xa0, 0x01, 0x40, 0x20, 0x05, 0xa1, 0xfe, 0x5a, 0xee, 0x46, 0xc1, 0xae, 0x55, 0x04}
			}
			if len(pps) == 0 {
				pps = []byte{0x44, 0x01, 0xc0, 0x73, 0xc0, 0x4c, 0x90}
			}

			var width, height uint16
			if s := h265.DecodeSPS(sps); s != nil {
				width = s.Width()
				height = s.Height()
			} else {
				width = 1920
				height = 1080
			}

			mv.WriteVideoTrack(
				uint32(i+1), codec.Name, codec.ClockRate, width, height, h265.EncodeConfig(vps, sps, pps),
			)

		case core.CodecJPEG:
			// Dimensions come from each frame's SOF; players and FFmpeg read those.
			mv.WriteVideoTrack(uint32(i+1), codec.Name, codec.ClockRate, 0, 0, nil)

		case core.CodecAAC:
			s := core.Between(codec.FmtpLine, "config=", ";")
			b, err := hex.DecodeString(s)
			if err != nil {
				return nil, err
			}

			mv.WriteAudioTrack(
				uint32(i+1), codec.Name, codec.ClockRate, uint16(codec.Channels), b,
			)

		case core.CodecOpus, core.CodecMP3, core.CodecPCMA, core.CodecPCMU, core.CodecFLAC:
			mv.WriteAudioTrack(
				uint32(i+1), codec.Name, codec.ClockRate, uint16(codec.Channels), nil,
			)
		}
	}

	mv.StartAtom(iso.MoovMvex)
	for i := range m.codecs {
		mv.WriteTrackExtend(uint32(i + 1))
	}
	mv.EndAtom() // MVEX

	mv.EndAtom() // MOOV

	return mv.Bytes(), nil
}

func (m *Muxer) Reset() {
	m.index = 0
	m.timingClock = 0
	for i := range m.dts {
		m.dts[i] = 0
		m.pts[i] = 0
		m.jpeg[i] = jpegClock{}
	}
}

func (m *Muxer) GetPayload(trackID byte, packet *rtp.Packet) []byte {
	codec := m.codecs[trackID]

	m.index++

	if codec.Name == core.CodecJPEG {
		return m.jpegPayload(trackID, packet)
	}

	timing, timed := core.GetSampleTiming(packet)
	duration := packet.Timestamp - m.pts[trackID]
	m.pts[trackID] = packet.Timestamp

	// flags important for Apple Finder video preview
	var flags uint32

	switch codec.Name {
	case core.CodecH264:
		if h264.IsKeyframe(packet.Payload) {
			flags = iso.SampleVideoIFrame
		} else {
			flags = iso.SampleVideoNonIFrame
		}
	case core.CodecH265:
		if h265.IsKeyframe(packet.Payload) {
			flags = iso.SampleVideoIFrame
		} else {
			flags = iso.SampleVideoNonIFrame
		}
	case core.CodecAAC:
		duration = 1024         // important for Apple Finder and QuickTime
		flags = iso.SampleAudio // not important?
	default:
		flags = iso.SampleAudio // important for FLAC on Android Telegram
	}

	// minumum duration important for MSE in Apple Safari
	if duration == 0 || duration > codec.ClockRate {
		duration = codec.ClockRate/1000 + 1
		m.pts[trackID] += duration
	}

	decodeTime := m.dts[trackID]
	composition := uint32(packet.ExtensionProfile)
	if timed {
		if m.timingClock == 0 || timing.ClockID != m.timingID {
			// Source IDs are monotonic modulo uint32. Ignore queued packets from
			// a retired source after a reconnect, without retaining an ID history.
			if m.timingClock != 0 && int32(timing.ClockID-m.timingID) < 0 {
				return nil
			}
			m.timingBase = 0
			for i, end := range m.dts {
				scaled, ok := rescaleTime(end, m.codecs[i].ClockRate, codec.ClockRate)
				if !ok {
					return nil
				}
				m.timingBase = max(m.timingBase, scaled)
			}
			m.timingEpoch = timing.DecodeTime
			m.timingClock = codec.ClockRate
			m.timingID = timing.ClockID
		}
		epoch, ok := rescaleTime(m.timingEpoch, m.timingClock, codec.ClockRate)
		if !ok || timing.DecodeTime < epoch {
			return nil
		}
		base, ok := rescaleTime(m.timingBase, m.timingClock, codec.ClockRate)
		if !ok {
			return nil
		}
		decodeTime = base + timing.DecodeTime - epoch
		if decodeTime < m.dts[trackID] {
			return nil
		}
		duration = timing.Duration
		composition = timing.CompositionOffset
	}
	size := len(packet.Payload)

	mv := iso.NewMovie(1024 + size)
	mv.WriteMovieFragment(
		m.index, uint32(trackID+1), duration, uint32(size), flags, decodeTime, composition,
	)
	mv.WriteData(packet.Payload)

	//log.Printf("[MP4] idx:%3d trk:%d dts:%6d cts:%4d dur:%5d time:%10d len:%5d", m.index, trackID+1, m.dts[trackID], packet.SSRC, duration, packet.Timestamp, len(packet.Payload))

	m.dts[trackID] = decodeTime + uint64(duration)

	return mv.Bytes()
}

// jpegPayload places each JPEG on its source clock: go2rtc's receive time for multipart
// JPEG, the camera's RTP clock for RTP JPEG. Unlike the generic path, gaps longer than a
// second are kept rather than collapsed, and a frame starts when it arrived.
func (m *Muxer) jpegPayload(trackID byte, packet *rtp.Packet) []byte {
	codec := m.codecs[trackID]
	clock := &m.jpeg[trackID]
	nominal := codec.ClockRate / 25
	now := time.Since(processStart)
	if m.now != nil {
		now = m.now()
	}
	decodeTime := m.dts[trackID]
	if clock.seen {
		delta := jpegDelta(packet.Timestamp-m.pts[trackID], now-clock.at, codec.ClockRate)
		decodeTime += delta
		clock.interval = uint32(min(delta, uint64(nominal)))
	} else {
		clock.interval = nominal
	}
	clock.seen = true
	clock.at = now
	m.pts[trackID] = packet.Timestamp
	m.dts[trackID] = decodeTime

	// The next frame's tfdt ends this one. Capping the duration keeps a stall out of
	// the frame after it, which would otherwise overlap the frames that follow.
	size := len(packet.Payload)
	mv := iso.NewMovie(1024 + size)
	mv.WriteMovieFragment(m.index, uint32(trackID+1), clock.interval, uint32(size), iso.SampleVideoIFrame, decodeTime, 0)
	mv.WriteData(packet.Payload)
	return mv.Bytes()
}

// jpegResetSkew is how far the source clock may disagree with the muxer's own clock.
// Queueing shifts consecutive frames by far less; a reset source clock is almost always
// hours out.
const jpegResetSkew = time.Minute

// jpegDelta unwraps a 32-bit source-clock interval using the time the muxer waited, so
// wraps and gaps longer than half the clock range survive. When the source clock was
// reset (an RTP reconnect or a wall clock step), the muxer's elapsed time stands in.
func jpegDelta(raw uint32, waited time.Duration, clockRate uint32) uint64 {
	elapsed := int64(waited) * int64(clockRate) / int64(time.Second)
	delta := int64(raw) + (elapsed-int64(raw)+1<<31)>>32<<32
	skew := int64(jpegResetSkew) * int64(clockRate) / int64(time.Second)
	if delta <= 0 || delta-elapsed > skew || elapsed-delta > skew {
		delta = max(elapsed, 1)
	}
	return uint64(delta)
}
