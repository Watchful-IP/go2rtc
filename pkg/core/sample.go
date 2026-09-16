package core

import "encoding/binary"

// SampleTiming is expressed in the track's codec clock, before RTP timestamp wrap.
type SampleTiming struct {
	ClockID           uint32 // core.NewID for the source clock, shared across tracks
	DecodeTime        uint64
	Duration          uint32
	CompositionOffset uint32
}

const sampleTimingProfile = 0x4754

// SetSampleTiming attaches local metadata to a version-zero raw media packet.
// RTP payloaders construct fresh version-two headers and do not transmit it.
func SetSampleTiming(packet *Packet, timing SampleTiming) {
	data := make([]byte, 20)
	binary.BigEndian.PutUint64(data, timing.DecodeTime)
	binary.BigEndian.PutUint32(data[8:], timing.Duration)
	binary.BigEndian.PutUint32(data[12:], timing.CompositionOffset)
	binary.BigEndian.PutUint32(data[16:], timing.ClockID)
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
	if len(data) != 20 {
		return SampleTiming{}, false
	}
	timing := SampleTiming{ClockID: binary.BigEndian.Uint32(data[16:]), DecodeTime: binary.BigEndian.Uint64(data), Duration: binary.BigEndian.Uint32(data[8:]), CompositionOffset: binary.BigEndian.Uint32(data[12:])}
	return timing, timing.Duration != 0
}
