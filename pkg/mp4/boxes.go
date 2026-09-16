package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var errInvalidMP4 = errors.New("mp4: invalid or truncated box")

// Keep offsets in the original segment; trun offsets are not relative to mdat.
type box struct {
	name                string
	start, end, payload int
	data                []byte
}

func boxes(data []byte, start, end int) ([]box, error) {
	var out []box
	for start < end {
		if end-start < 8 {
			return nil, errInvalidMP4
		}
		size, header := uint64(binary.BigEndian.Uint32(data[start:])), 8
		if size == 1 {
			if end-start < 16 {
				return nil, errInvalidMP4
			}
			size, header = binary.BigEndian.Uint64(data[start+8:]), 16
		} else if size == 0 {
			size = uint64(end - start)
		}
		if size < uint64(header) || size > uint64(end-start) {
			return nil, errInvalidMP4
		}
		next := start + int(size)
		out = append(out, box{string(data[start+4 : start+8]), start, next, start + header, data[start+header : next]})
		if len(out) > 65536 {
			return nil, errors.New("mp4: too many boxes")
		}
		start = next
	}
	return out, nil
}

func children(data []byte, b box) ([]box, error) { return boxes(data, b.payload, b.end) }

func child(data []byte, b box, name string) (box, error) {
	bs, err := children(data, b)
	if err != nil {
		return box{}, err
	}
	for _, v := range bs {
		if v.name == name {
			return v, nil
		}
	}
	return box{}, fmt.Errorf("mp4: missing %s", name)
}

type fields struct {
	data []byte
	err  error
}

func (r *fields) u32() uint32 {
	if len(r.data) < 4 {
		r.err = errInvalidMP4
		return 0
	}
	v := binary.BigEndian.Uint32(r.data)
	r.data = r.data[4:]
	return v
}
func (r *fields) u64() uint64 { hi := r.u32(); lo := r.u32(); return uint64(hi)<<32 | uint64(lo) }
