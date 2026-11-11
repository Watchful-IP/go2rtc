package milestone

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

// GenericByteData audio codec IDs and G.711 sub-types.
// https://doc.developer.milestonesys.com/mipsdk/reference/protocols/audio_sub_formats.html
const (
	audioG711     = 0x0003
	audioAACMPEG2 = 0x0011
	audioAACMPEG4 = 0x0012

	g711MuLaw = 1
	g711ALaw  = 2

	audioHeaderSize = 42
	maxAudioDrift   = 200 // ms the sample count may part from recorder time
)

// audioPacket is one microphone packet. Time is recorder UTC milliseconds.
type audioPacket struct {
	codec   uint16
	subtype uint16
	time    int64
	data    []byte
}

// parseAudio extracts audio packets from one response part.
func parseAudio(b []byte, packets []audioPacket) ([]audioPacket, error) {
	err := walkPackets(b, func(dataType uint16, p []byte) error {
		if dataType != gbdAudioPacket {
			return nil
		}
		if len(p) < audioHeaderSize {
			return errDesync
		}
		packets = append(packets, audioPacket{
			codec:   binary.BigEndian.Uint16(p[6:]),
			subtype: binary.BigEndian.Uint16(p[30:]),
			time:    int64(binary.BigEndian.Uint64(p[12:])),
			data:    p[audioHeaderSize:],
		})
		return nil
	})
	return packets, err
}

// audioCodec is the track codec for a microphone's packets.
func audioCodec(p *audioPacket) (*core.Codec, error) {
	switch p.codec {
	case audioAACMPEG2, audioAACMPEG4:
		if codec := aac.ADTSToCodec(p.data); codec != nil {
			codec.PayloadType = core.PayloadTypeRAW
			return codec, nil
		}
		return nil, errors.New("milestone: AAC audio without an ADTS header")
	case audioG711:
		switch p.subtype {
		case g711MuLaw:
			return &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}, nil
		case g711ALaw:
			return &core.Codec{Name: core.CodecPCMA, ClockRate: 8000}, nil
		}
	}
	return nil, fmt.Errorf("milestone: unsupported audio codec 0x%04X/%d", p.codec, p.subtype)
}

// audioUnit is one codec frame and its length in samples.
type audioUnit struct {
	data    []byte
	samples uint32
}

// units splits a packet into codec frames.
func (p *audioPacket) units(codec *core.Codec) []audioUnit {
	if codec.Name != core.CodecAAC {
		return []audioUnit{{p.data, uint32(len(p.data))}}
	}
	var out []audioUnit
	for b := p.data; aac.IsADTS(b); {
		size := int(aac.ReadADTSSize(b))
		header := aac.ADTSHeaderLen(b)
		if size <= header || size > len(b) {
			break
		}
		out = append(out, audioUnit{b[header:size], aac.AUTime})
		b = b[size:]
	}
	return out
}

// liveAudio is a camera's related microphone, read over its own ImageServer connection.
type liveAudio struct {
	client *Client
	id     uint16 // GenericByteData codec of the first packet
	codec  *core.Codec
	recv   *core.Receiver
	first  []audioPacket // read while probing

	next     int64 // decode time of the next sample, codec clock
	anchored bool
}

// dialAudio connects to the microphone and learns its codec.
func dialAudio(src *Source) (*liveAudio, error) {
	mic := *src
	mic.Camera, mic.Stream = src.Mic, ""
	client, err := dialClient(&mic, false)
	if err != nil {
		return nil, err
	}
	a := &liveAudio{client: client}
	if err = a.probe(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return a, nil
}

func (a *liveAudio) probe() error {
	if err := a.client.Live(); err != nil {
		return err
	}
	for {
		res, err := a.client.read()
		if err != nil {
			return err
		}
		if res.isXML() {
			if liveStatus(res.xml)[statusConnectionLost] == 1 {
				return errMicOffline
			}
			continue
		}
		if a.first, err = a.packets(res); err != nil {
			return err
		}
		if len(a.first) > 0 {
			a.id = a.first[0].codec
			a.codec, err = audioCodec(&a.first[0])
			return err
		}
	}
}

var errMicOffline = errors.New("milestone: microphone is not connected to the recording server")

func (a *liveAudio) packets(res *response) ([]audioPacket, error) {
	var out []audioPacket
	var err error
	for _, part := range res.parts {
		if out, err = parseAudio(part, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// run plays the microphone from the first video picture onwards.
func (a *liveAudio) run(l *Live) {
	select {
	case <-l.videoStarted:
	case <-l.done:
		return
	}
	for _, p := range a.first {
		a.write(l, &p)
	}
	a.first = nil
	for {
		res, err := a.client.read()
		if err != nil {
			return
		}
		if res.isXML() {
			continue
		}
		ps, err := a.packets(res)
		if err != nil {
			return
		}
		for i := range ps {
			a.write(l, &ps[i])
		}
	}
}

// write stamps a packet on the video's timeline, samples back to back.
func (a *liveAudio) write(l *Live, p *audioPacket) {
	if p.time < l.base || p.codec != a.id {
		return
	}
	rate := int64(a.codec.ClockRate)
	at := (p.time - l.base) * rate / 1000
	drift := rate * maxAudioDrift / 1000
	switch {
	case !a.anchored || at > a.next+drift:
		a.next, a.anchored = at, true
	case a.next > at+drift:
		return // the microphone's clock runs ahead of the recorder's
	}
	for _, u := range p.units(a.codec) {
		packet := &core.Packet{Header: rtp.Header{Timestamp: uint32(a.next)}, Payload: u.data}
		core.SetSampleTiming(packet, core.SampleTiming{ClockID: l.ID, DecodeTime: uint64(a.next), Duration: u.samples})
		a.recv.Input(packet)
		a.next += int64(u.samples)
	}
}
