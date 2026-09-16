package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

// Probe installs tracks atomically; malformed replacement init data cannot poison state.
func (d *Demuxer) Probe(init []byte) ([]*core.Media, error) {
	if len(init) > 8<<20 {
		return nil, errors.New("mp4: init exceeds size limit")
	}
	bs, err := boxes(init, 0, len(init))
	if err != nil {
		return nil, err
	}
	tracks := make(map[uint32]*track)
	var medias []*core.Media
	var trexs []box
	for _, moov := range bs {
		if moov.name != "moov" {
			continue
		}
		cs, err := children(init, moov)
		if err != nil {
			return nil, err
		}
		for _, b := range cs {
			switch b.name {
			case "trak":
				id, t, err := probeTrack(init, b)
				if err != nil {
					return nil, err
				}
				if id == 0 || tracks[id] != nil {
					return nil, errors.New("mp4: invalid or duplicate track ID")
				}
				tracks[id] = t
				if len(tracks) > 32 {
					return nil, errors.New("mp4: too many tracks")
				}
				if t.codec != nil {
					medias = append(medias, &core.Media{Kind: t.codec.Kind(), Direction: core.DirectionRecvonly, Codecs: []*core.Codec{t.codec}})
				}
			case "mvex":
				v, err := children(init, b)
				if err != nil {
					return nil, err
				}
				trexs = append(trexs, v...)
			}
		}
	}
	for _, b := range trexs {
		if b.name != "trex" {
			continue
		}
		r := fields{data: b.data}
		if r.u32() != 0 {
			return nil, errInvalidMP4
		}
		id := r.u32()
		t := tracks[id]
		if t == nil {
			return nil, errors.New("mp4: trex references unknown track")
		}
		t.description = r.u32()
		t.duration = r.u32()
		t.size = r.u32()
		r.u32()
		if r.err != nil {
			return nil, r.err
		}
	}
	if len(medias) == 0 {
		return nil, errors.New("mp4: no supported tracks")
	}
	d.tracks = tracks
	return medias, nil
}

func probeTrack(data []byte, b box) (uint32, *track, error) {
	tkhd, err := child(data, b, "tkhd")
	if err != nil {
		return 0, nil, err
	}
	id, err := timedField(tkhd.data)
	if err != nil {
		return 0, nil, err
	}
	mdia, err := child(data, b, "mdia")
	if err != nil {
		return 0, nil, err
	}
	handler, err := child(data, mdia, "hdlr")
	if err != nil || len(handler.data) < 12 {
		return 0, nil, errInvalidMP4
	}
	if kind := string(handler.data[8:12]); kind != "vide" && kind != "soun" {
		return id, &track{scale: 1, description: 1}, nil
	}
	mdhd, err := child(data, mdia, "mdhd")
	if err != nil {
		return 0, nil, err
	}
	scale, err := timedField(mdhd.data)
	if err != nil {
		return 0, nil, err
	}
	if scale == 0 {
		return 0, nil, errors.New("mp4: zero timescale")
	}
	t := &track{scale: scale, description: 1}
	stsd := mdia
	for _, name := range []string{"minf", "stbl", "stsd"} {
		stsd, err = child(data, stsd, name)
		if err != nil {
			return 0, nil, err
		}
	}
	if len(stsd.data) < 8 || binary.BigEndian.Uint32(stsd.data[4:]) != 1 {
		return 0, nil, errors.New("mp4: expected one sample description")
	}
	entries, err := boxes(data, stsd.payload+8, stsd.end)
	if err != nil || len(entries) != 1 {
		return 0, nil, errInvalidMP4
	}
	entry := entries[0]
	var skip int
	switch entry.name {
	case "avc1", "avc3", "hvc1", "hev1":
		skip = 78
	case "mp4a":
		skip = 28
	case "encv", "enca":
		return 0, nil, errors.New("mp4: encrypted tracks require ffmpeg")
	default:
		return id, t, nil
	}
	if len(entry.data) < skip {
		return 0, nil, errInvalidMP4
	}
	cs, err := boxes(data, entry.payload+skip, entry.end)
	if err != nil {
		return 0, nil, err
	}
	for _, c := range cs {
		switch {
		case (entry.name == "avc1" || entry.name == "avc3") && c.name == "avcC":
			conf, err := avcConfig(c.data)
			if err != nil {
				return 0, nil, err
			}
			t.codec = h264.ConfigToCodec(conf)
			t.nalLength = int(c.data[4]&3) + 1
		case (entry.name == "hvc1" || entry.name == "hev1") && c.name == "hvcC":
			conf, err := hevcConfig(c.data)
			if err != nil {
				return 0, nil, err
			}
			t.codec = h265.ConfigToCodec(conf)
			t.nalLength = int(c.data[21]&3) + 1
		case entry.name == "mp4a" && c.name == "esds":
			if len(c.data) < 4 {
				return 0, nil, errInvalidMP4
			}
			conf, err := audioConfig(c.data[4:], 0)
			if err != nil {
				return 0, nil, err
			}
			// Limit to AAC-LC, whose sample duration and channel model the muxer supports.
			if len(conf) < 2 || conf[0]>>3 != 2 || (conf[0]&7)<<1|conf[1]>>7 >= 13 || conf[1]>>3&15 == 0 || conf[1]&7 != 0 {
				return 0, nil, errors.New("mp4: unsupported AAC configuration")
			}
			t.codec = aac.ConfigToCodec(conf)
		}
	}
	if t.codec == nil {
		return 0, nil, fmt.Errorf("mp4: missing codec configuration for %s", entry.name)
	}
	return id, t, nil
}

func timedField(data []byte) (uint32, error) {
	if len(data) < 4 {
		return 0, errInvalidMP4
	}
	offset := 12
	switch data[0] {
	case 0:
	case 1:
		offset = 20
	default:
		return 0, errInvalidMP4
	}
	if len(data) < offset+4 {
		return 0, errInvalidMP4
	}
	return binary.BigEndian.Uint32(data[offset:]), nil
}

func avcConfig(data []byte) ([]byte, error) {
	if len(data) < 7 || data[0] != 1 {
		return nil, errInvalidMP4
	}
	p := 6
	var sets [2][]byte
	counts := []int{int(data[5] & 31), 0}
	for k := 0; k < 2; k++ {
		if k == 1 {
			if p >= len(data) {
				return nil, errInvalidMP4
			}
			counts[k] = int(data[p])
			p++
		}
		for i := 0; i < counts[k]; i++ {
			if p+2 > len(data) {
				return nil, errInvalidMP4
			}
			n := int(binary.BigEndian.Uint16(data[p:]))
			p += 2
			if n == 0 || n > len(data)-p {
				return nil, errInvalidMP4
			}
			if sets[k] == nil {
				sets[k] = data[p : p+n]
			}
			p += n
		}
	}
	if len(sets[0]) < 4 || len(sets[1]) == 0 {
		return nil, errors.New("mp4: missing H264 parameter sets")
	}
	return h264.EncodeConfig(sets[0], sets[1]), nil
}

func hevcConfig(data []byte) ([]byte, error) {
	if len(data) < 23 || data[0] != 1 {
		return nil, errInvalidMP4
	}
	var sets [3][]byte
	p := 23
	for i := 0; i < int(data[22]); i++ {
		if p+3 > len(data) {
			return nil, errInvalidMP4
		}
		kind := int(data[p]&63) - 32
		n := int(binary.BigEndian.Uint16(data[p+1:]))
		p += 3
		for j := 0; j < n; j++ {
			if p+2 > len(data) {
				return nil, errInvalidMP4
			}
			size := int(binary.BigEndian.Uint16(data[p:]))
			p += 2
			if size == 0 || size > len(data)-p {
				return nil, errInvalidMP4
			}
			if kind >= 0 && kind < 3 && sets[kind] == nil {
				sets[kind] = data[p : p+size]
			}
			p += size
		}
	}
	conf := h265.EncodeConfig(sets[0], sets[1], sets[2])
	if conf == nil {
		return nil, errors.New("mp4: missing H265 parameter sets")
	}
	return conf, nil
}

// ISO 14496-1 descriptors have variable-length (1 to 4 byte) sizes.
func audioConfig(data []byte, depth int) ([]byte, error) {
	if depth > 3 {
		return nil, errInvalidMP4
	}
	for len(data) > 0 {
		tag := data[0]
		data = data[1:]
		n := 0
		done := false
		for i := 0; i < 4 && len(data) > 0; i++ {
			v := data[0]
			data = data[1:]
			n = n<<7 | int(v&127)
			if v&128 == 0 {
				done = true
				break
			}
		}
		if !done || n > len(data) {
			return nil, errInvalidMP4
		}
		body := data[:n]
		data = data[n:]
		skip := 0
		switch tag {
		case 5:
			return body, nil
		case 3:
			if len(body) < 3 {
				return nil, errInvalidMP4
			}
			flags := body[2]
			skip = 3
			if flags&128 != 0 {
				skip += 2
			}
			if flags&64 != 0 {
				if skip >= len(body) {
					return nil, errInvalidMP4
				}
				skip += 1 + int(body[skip])
			}
			if flags&32 != 0 {
				skip += 2
			}
		case 4:
			skip = 13
		default:
			continue
		}
		if skip > len(body) {
			return nil, errInvalidMP4
		}
		conf, err := audioConfig(body[skip:], depth+1)
		if err != nil {
			return nil, err
		}
		if conf != nil {
			return conf, nil
		}
	}
	return nil, nil
}

func normalizeNALs(data []byte, length, minSize int) ([]byte, error) {
	if length == 0 {
		return data, nil
	}
	var out []byte
	for p := 0; p < len(data); {
		if len(data)-p < length {
			return nil, errors.New("mp4: truncated NAL length")
		}
		n := uint32(0)
		for _, v := range data[p : p+length] {
			n = n<<8 | uint32(v)
		}
		p += length
		if n < uint32(minSize) || uint64(n) > uint64(len(data)-p) {
			return nil, errors.New("mp4: invalid NAL size")
		}
		if length != 4 {
			if len(out)+4+int(n) > 64<<20 {
				return nil, errors.New("mp4: normalized sample exceeds size limit")
			}
			out = binary.BigEndian.AppendUint32(out, n)
			out = append(out, data[p:p+int(n)]...)
		}
		p += int(n)
	}
	if length == 4 {
		return data, nil
	}
	return out, nil
}
