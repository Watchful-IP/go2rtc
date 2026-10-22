package exacq

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	headerSize     = 32
	maxPacketSize  = 32 << 20
	maxDecodedSize = 32 << 20
)

const (
	packetDiscover uint32 = 1
	packetLogin    uint32 = 2
	packetStatus   uint32 = 3
	packetConfig   uint32 = 4
	packetLive     uint32 = 5
	packetStored   uint32 = 6
	packetKey      uint32 = 7
	packetPTZ      uint32 = 8
	packetLicense  uint32 = 9
	packetBinary   uint32 = 10
)

// packet is the on-wire tagCommHeader followed by its payload. Length includes
// the 32-byte header. No host C struct layout is used to decode network data.
type packet struct {
	Type        uint32
	Subtype     uint32
	Status      uint16
	Compression uint8
	Encryption  uint8
	Reserved    [16]byte
	Payload     []byte
}

func readPacket(r io.Reader) (*packet, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(header[:4])
	if n < headerSize || n > maxPacketSize {
		return nil, fmt.Errorf("exacq: invalid packet length %d", n)
	}
	p := &packet{
		Type:        binary.LittleEndian.Uint32(header[4:8]),
		Subtype:     binary.LittleEndian.Uint32(header[8:12]),
		Status:      binary.LittleEndian.Uint16(header[12:14]),
		Compression: header[14], Encryption: header[15],
		Payload: make([]byte, int(n)-headerSize),
	}
	copy(p.Reserved[:], header[16:])
	if _, err := io.ReadFull(r, p.Payload); err != nil {
		return nil, err
	}
	return p, nil
}

func writePacket(w io.Writer, p *packet) error {
	if len(p.Payload) > maxPacketSize-headerSize {
		return errors.New("exacq: packet exceeds size limit")
	}
	var header [headerSize]byte
	binary.LittleEndian.PutUint32(header[:4], uint32(headerSize+len(p.Payload)))
	binary.LittleEndian.PutUint32(header[4:8], p.Type)
	binary.LittleEndian.PutUint32(header[8:12], p.Subtype)
	binary.LittleEndian.PutUint16(header[12:14], p.Status)
	header[14], header[15] = p.Compression, p.Encryption
	copy(header[16:], p.Reserved[:])
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	return writeFull(w, p.Payload)
}

func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func decompressPayload(b []byte, compression uint8) ([]byte, error) {
	switch compression {
	case 0:
		return b, nil
	case 1:
		r, err := zlib.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("exacq: compressed payload: %w", err)
		}
		defer r.Close()
		decoded, err := io.ReadAll(io.LimitReader(r, maxDecodedSize+1))
		if err != nil {
			return nil, fmt.Errorf("exacq: compressed payload: %w", err)
		}
		if len(decoded) > maxDecodedSize {
			return nil, errors.New("exacq: decompressed payload exceeds size limit")
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("exacq: unsupported compression %d", compression)
	}
}

func compressPayload(b []byte) ([]byte, error) {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
