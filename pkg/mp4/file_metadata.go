package mp4

import (
	"encoding/binary"
	"errors"
)

// Only sample-index boxes reach mp4ff. Optional vendor metadata can contain
// arbitrary container nesting; it isn't needed for native sample extraction.
func sampleIndex(data []byte) ([]byte, error) {
	bs, err := boxes(data, 0, len(data))
	if err != nil || len(bs) != 1 || bs[0].name != "moov" {
		return nil, errInvalidMP4
	}
	return indexContainer(data, bs[0])
}

func indexContainer(data []byte, b box) ([]byte, error) {
	layouts := map[string]string{
		"moov": "mvhd trak", "trak": "tkhd edts mdia", "edts": "elst",
		"mdia": "mdhd hdlr minf", "minf": "stbl",
		"stbl": "stts ctts stsc stsz stss stco co64 saio saiz",
	}
	allowed := layouts[b.name]
	cs, err := children(data, b)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 8)
	copy(out[4:], b.name)
	for _, c := range cs {
		if b.name == "moov" && c.name == "mvex" {
			return nil, errors.New("mp4: fragmented movie requires a different input")
		}
		keep := false
		for i := 0; i+4 <= len(allowed); i += 5 {
			if allowed[i:i+4] == c.name {
				keep = true
				break
			}
		}
		if !keep {
			continue
		}
		if _, container := layouts[c.name]; container {
			encoded, err := indexContainer(data, c)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded...)
		} else {
			out = append(out, data[c.start:c.end]...)
		}
	}
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	return out, nil
}

func fileReferences(data []byte) error {
	bs, err := boxes(data, 0, len(data))
	if err != nil || len(bs) != 1 {
		return errInvalidMP4
	}
	traks, err := children(data, bs[0])
	if err != nil {
		return err
	}
	for _, trak := range traks {
		if trak.name != "trak" {
			continue
		}
		mdia, err := child(data, trak, "mdia")
		if err != nil {
			return err
		}
		hdlr, err := child(data, mdia, "hdlr")
		if err != nil || len(hdlr.data) < 12 {
			return errInvalidMP4
		}
		if kind := string(hdlr.data[8:12]); kind != "vide" && kind != "soun" {
			continue
		}
		minf, err := child(data, mdia, "minf")
		if err != nil {
			return err
		}
		dinf, err := child(data, minf, "dinf")
		if err != nil {
			return err
		}
		dref, err := child(data, dinf, "dref")
		if err != nil || len(dref.data) < 8 || binary.BigEndian.Uint32(dref.data[4:]) != 1 {
			return errors.New("mp4: external data references unsupported")
		}
		refs, err := boxes(data, dref.payload+8, dref.end)
		if err != nil || len(refs) != 1 || refs[0].name != "url " || len(refs[0].data) != 4 || binary.BigEndian.Uint32(refs[0].data) != 1 {
			return errors.New("mp4: external data references unsupported")
		}
		stbl, err := child(data, minf, "stbl")
		if err != nil {
			return err
		}
		stsd, err := child(data, stbl, "stsd")
		if err != nil || len(stsd.data) < 8 {
			return errInvalidMP4
		}
		entries, err := boxes(data, stsd.payload+8, stsd.end)
		if err != nil || len(entries) != 1 || len(entries[0].data) < 8 || binary.BigEndian.Uint16(entries[0].data[6:8]) != 1 {
			return errors.New("mp4: invalid sample data reference")
		}
	}
	return nil
}
