package exacq

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestPacketFraming(t *testing.T) {
	p := &packet{Type: packetLive, Subtype: 2, Status: 3, Compression: 1, Encryption: 1, Payload: []byte("payload")}
	p.Reserved[15] = 4
	var b bytes.Buffer
	if err := writePacket(&b, p); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(b.Bytes()); got != 39 {
		t.Fatalf("length %d", got)
	}
	decoded, err := readPacket(&shortReader{Reader: &b})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != p.Type || decoded.Subtype != p.Subtype || decoded.Status != p.Status || decoded.Compression != p.Compression || decoded.Encryption != p.Encryption || decoded.Reserved != p.Reserved || !bytes.Equal(decoded.Payload, p.Payload) {
		t.Fatalf("decoded packet: %+v", decoded)
	}
}

type shortReader struct{ io.Reader }

func (r *shortReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

func TestPacketRejectsMalformedLengths(t *testing.T) {
	for _, n := range []uint32{0, 31, maxPacketSize + 1, ^uint32(0)} {
		var header [32]byte
		binary.LittleEndian.PutUint32(header[:], n)
		if _, err := readPacket(bytes.NewReader(header[:])); err == nil {
			t.Fatalf("accepted length %d", n)
		}
	}
	for n := 0; n < 33; n++ {
		b := make([]byte, 33)
		binary.LittleEndian.PutUint32(b, 33)
		if _, err := readPacket(bytes.NewReader(b[:n])); err == nil {
			t.Fatalf("accepted truncated packet %d", n)
		}
	}
}

func TestCompressionBounds(t *testing.T) {
	plain := bytes.Repeat([]byte("<eDVR>test</eDVR>"), 100)
	b, err := compressPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decompressPayload(b, 1)
	if err != nil || !bytes.Equal(plain, got) {
		t.Fatalf("roundtrip: %v", err)
	}
	for _, bad := range [][]byte{nil, {1, 2, 3}, b[:len(b)-1]} {
		if _, err = decompressPayload(bad, 1); err == nil {
			t.Fatal("accepted corrupt zlib")
		}
	}
	if _, err = decompressPayload(b, 2); err == nil {
		t.Fatal("accepted unknown compression")
	}
}

func FuzzReadPacket(f *testing.F) {
	var b bytes.Buffer
	_ = writePacket(&b, &packet{Type: packetDiscover, Payload: []byte("<eDVR/>")})
	f.Add(b.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := readPacket(bytes.NewReader(data))
		if err == nil && len(p.Payload) > maxPacketSize-headerSize {
			t.Fatal("unbounded payload")
		}
	})
}
