package core

import "encoding/binary"

// SampleTiming is expressed in the track's codec clock, before RTP timestamp wrap.
type SampleTiming struct {
	ClockID               uint32 // core.NewID for the source clock, shared across tracks
	DecodeTime            uint64
	Duration              uint32
	CompositionOffset     uint32
	SourceTimeUncertainty uint32 // ticks; only for reconstructed cached-prefix timing
	SourceTimeUnixNano    int64  // optional absolute decode clock; zero for relative-only sources
}

const sampleTimingProfile = 0x4754

// SetSampleTiming attaches local metadata to a version-zero raw media packet.
// RTP payloaders construct fresh version-two headers and do not transmit it.
func SetSampleTiming(packet *Packet, timing SampleTiming) {
	data := make([]byte, 32)
	binary.BigEndian.PutUint64(data, timing.DecodeTime)
	binary.BigEndian.PutUint32(data[8:], timing.Duration)
	binary.BigEndian.PutUint32(data[12:], timing.CompositionOffset)
	binary.BigEndian.PutUint32(data[16:], timing.ClockID)
	binary.BigEndian.PutUint64(data[20:], uint64(timing.SourceTimeUnixNano))
	binary.BigEndian.PutUint32(data[28:], timing.SourceTimeUncertainty)
	packet.Version = 0
	packet.PayloadType = PayloadTypeRAW
	packet.Extension = true
	packet.ExtensionProfile = sampleTimingProfile
	// A shallow packet clone can share Extensions; never overwrite its backing array.
	packet.Extensions = nil
	_ = packet.SetExtension(0, data)
}

func GetSampleTiming(packet *Packet) (SampleTiming, bool) {
	if packet.Version != 0 || packet.PayloadType != PayloadTypeRAW || !packet.Extension || packet.ExtensionProfile != sampleTimingProfile {
		return SampleTiming{}, false
	}
	data := packet.GetExtension(0)
	if len(data) != 20 && len(data) != 28 && len(data) != 32 {
		return SampleTiming{}, false
	}
	timing := SampleTiming{ClockID: binary.BigEndian.Uint32(data[16:]), DecodeTime: binary.BigEndian.Uint64(data), Duration: binary.BigEndian.Uint32(data[8:]), CompositionOffset: binary.BigEndian.Uint32(data[12:])}
	if len(data) >= 28 {
		timing.SourceTimeUnixNano = int64(binary.BigEndian.Uint64(data[20:]))
	}
	if len(data) == 32 {
		timing.SourceTimeUncertainty = binary.BigEndian.Uint32(data[28:])
	}
	return timing, timing.Duration != 0
}
