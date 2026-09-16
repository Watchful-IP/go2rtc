package mp4

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

type track struct {
	codec                              *core.Codec
	scale, duration, size, description uint32
	nalLength                          int
}

type Demuxer struct{ tracks map[uint32]*track }

type Sample struct {
	TrackID             uint32
	Packet              *core.Packet
	DecodeTime          uint64
	Duration, TimeScale uint32
}

func (d *Demuxer) GetTrackID(codec *core.Codec) uint32 {
	for id, t := range d.tracks {
		if t.codec == codec {
			return id
		}
	}
	return 0
}

// Demux validates the complete segment before exposing any samples.
func (d *Demuxer) Demux(data []byte) ([]Sample, error) {
	if len(data) > 64<<20 {
		return nil, errors.New("mp4: segment exceeds size limit")
	}
	bs, err := boxes(data, 0, len(data))
	if err != nil {
		return nil, err
	}
	var mdats []box
	for _, b := range bs {
		if b.name == "mdat" {
			mdats = append(mdats, b)
		}
	}
	var out []Sample
	for _, b := range bs {
		if b.name != "moof" {
			continue
		}
		trafs, err := children(data, b)
		if err != nil {
			return nil, err
		}
		base := int64(b.start)
		for _, traf := range trafs {
			if traf.name != "traf" {
				continue
			}
			samples, end, err := d.demuxTrack(data, traf, int64(b.start), base, mdats)
			if err != nil {
				return nil, err
			}
			base = end
			out = append(out, samples...)
			if len(out) > 65536 {
				return nil, errors.New("mp4: too many samples")
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("mp4: no supported samples")
	}
	return out, nil
}

func (d *Demuxer) demuxTrack(data []byte, traf box, moof, implicitBase int64, mdats []box) ([]Sample, int64, error) {
	bs, err := children(data, traf)
	if err != nil {
		return nil, 0, err
	}
	var tfhd, tfdt *box
	for i := range bs {
		switch bs[i].name {
		case "tfhd":
			if tfhd != nil {
				return nil, 0, errInvalidMP4
			}
			tfhd = &bs[i]
		case "tfdt":
			if tfdt != nil {
				return nil, 0, errInvalidMP4
			}
			tfdt = &bs[i]
		case "senc", "saiz", "saio":
			return nil, 0, errors.New("mp4: encrypted fragments require ffmpeg")
		}
	}
	if tfhd == nil || tfdt == nil {
		return nil, 0, errors.New("mp4: missing tfhd or tfdt")
	}
	r := fields{data: tfhd.data}
	flags := r.u32()
	id := r.u32()
	t := d.tracks[id]
	if t == nil {
		return nil, 0, fmt.Errorf("mp4: unknown track %d", id)
	}
	if flags>>24 != 0 || flags & ^uint32(0x03003b) != 0 {
		return nil, 0, errors.New("mp4: unsupported tfhd flags")
	}
	base := implicitBase
	if flags&0x020000 != 0 {
		base = moof
	}
	if flags&1 != 0 {
		v := r.u64()
		if v > uint64(len(data)) {
			return nil, 0, errors.New("mp4: base offset outside segment")
		}
		base = int64(v)
	}
	desc, duration, size := t.description, t.duration, t.size
	if flags&2 != 0 {
		desc = r.u32()
	}
	if desc != 1 {
		return nil, 0, errors.New("mp4: unsupported sample description")
	}
	if flags&8 != 0 {
		duration = r.u32()
	}
	if flags&16 != 0 {
		size = r.u32()
	}
	if flags&32 != 0 {
		r.u32()
	}
	if r.err != nil {
		return nil, 0, r.err
	}
	r = fields{data: tfdt.data}
	version := r.u32()
	var ts uint64
	switch version {
	case 0:
		ts = uint64(r.u32())
	case 1 << 24:
		ts = r.u64()
	default:
		return nil, 0, errors.New("mp4: unsupported tfdt version")
	}
	if r.err != nil {
		return nil, 0, r.err
	}
	pos := base
	var out []Sample
	for _, b := range bs {
		if b.name != "trun" {
			continue
		}
		r = fields{data: b.data}
		flags := r.u32()
		count := r.u32()
		version := flags >> 24
		flags &= 0xffffff
		if version > 1 || flags & ^uint32(0xf05) != 0 || flags&4 != 0 && flags&0x400 != 0 {
			return nil, 0, errors.New("mp4: unsupported trun flags")
		}
		if count > 65536 || len(out)+int(count) > 65536 {
			return nil, 0, errors.New("mp4: too many samples")
		}
		if flags&1 != 0 {
			pos = base + int64(int32(r.u32()))
		}
		if flags&4 != 0 {
			r.u32()
		}
		for i := uint32(0); i < count; i++ {
			dur, n := duration, size
			if flags&0x100 != 0 {
				dur = r.u32()
			}
			if flags&0x200 != 0 {
				n = r.u32()
			}
			if flags&0x400 != 0 {
				r.u32()
			}
			var cts int64
			if flags&0x800 != 0 {
				v := r.u32()
				cts = int64(v)
				if version == 1 {
					cts = int64(int32(v))
				}
			}
			if r.err != nil {
				return nil, 0, r.err
			}
			if dur == 0 || n == 0 || ts > math.MaxUint64-uint64(dur) {
				return nil, 0, errors.New("mp4: invalid sample duration or size")
			}
			end := pos + int64(n)
			index := sort.Search(len(mdats), func(i int) bool { return int64(mdats[i].end) > pos })
			valid := index < len(mdats) && pos >= int64(mdats[index].payload) && end <= int64(mdats[index].end)
			if !valid {
				return nil, 0, errors.New("mp4: sample outside mdat")
			}
			if t.codec != nil {
				// The downstream muxer stores CTS in a uint16 RTP extension field.
				if cts < 0 || uint64(cts)*uint64(t.codec.ClockRate)/uint64(t.scale) > math.MaxUint16 {
					return nil, 0, errors.New("mp4: composition offset requires ffmpeg")
				}
				minNAL := 1
				if t.codec.Name == core.CodecH265 {
					minNAL = 2
				}
				payload, err := normalizeNALs(data[pos:end], t.nalLength, minNAL)
				if err != nil {
					return nil, 0, err
				}
				offset := uint32(uint64(cts) * uint64(t.codec.ClockRate) / uint64(t.scale))
				if ts > math.MaxUint64-uint64(cts) {
					return nil, 0, errors.New("mp4: presentation time overflow")
				}
				timestamp := scaleTimestamp(ts+uint64(cts), t.codec.ClockRate, t.scale)
				out = append(out, Sample{id, &rtp.Packet{Header: rtp.Header{Timestamp: timestamp, ExtensionProfile: uint16(offset)}, Payload: payload}, ts, dur, t.scale})
			}
			pos = end
			ts += uint64(dur)
		}
		if r.err != nil {
			return nil, 0, r.err
		}
	}
	return out, pos, nil
}

// Divide before multiplying so long-running streams retain integer precision.
func scaleTimestamp(ts uint64, clock, scale uint32) uint32 {
	return uint32(ts/uint64(scale))*clock + uint32(ts%uint64(scale)*uint64(clock)/uint64(scale))
}

// Compatible permits refreshed initialization data without mutating receiver codecs.
func (d *Demuxer) Compatible(other *Demuxer) bool {
	if len(d.tracks) != len(other.tracks) {
		return false
	}
	for id, t := range d.tracks {
		o := other.tracks[id]
		if o == nil || t.scale != o.scale || (t.codec == nil) != (o.codec == nil) {
			return false
		}
		if t.codec != nil && *t.codec != *o.codec {
			return false
		}
	}
	return true
}
