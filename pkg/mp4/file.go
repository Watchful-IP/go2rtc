package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	ff "github.com/Eyevinn/mp4ff/mp4"
)

const maxFileSamples = 1_000_000
const maxFileSampleSize = 16 << 20

type fileSample struct {
	offset int64
	size   uint32
	timing core.SampleTiming
	at     time.Duration
}
type fileTrack struct {
	config  *track
	samples []fileSample
}
type fileExtent struct{ start, end int64 }

// FileProducer emits a finite progressive MP4 on the native encoded-sample graph.
// Samples are paced for playback; finite receivers apply backpressure without drops.
type FileProducer struct {
	core.Connection
	reader io.ReaderAt
	tracks []*fileTrack
	ctx    context.Context
	cancel context.CancelFunc
}

func OpenFile(r io.ReaderAt, size int64) (*FileProducer, error) {
	moov, extents, err := fileIndex(r, size)
	if err != nil {
		return nil, err
	}
	if err := fileReferences(moov); err != nil {
		return nil, err
	}
	dem := &Demuxer{}
	medias, err := dem.Probe(moov)
	if err != nil {
		return nil, err
	}
	tracks, err := fileTracks(moov, dem, extents)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &FileProducer{Connection: core.Connection{ID: core.NewID(), FormatName: "mp4/file", Medias: medias}, reader: r, tracks: tracks, ctx: ctx, cancel: cancel}, nil
}

func fileIndex(r io.ReaderAt, size int64) ([]byte, []fileExtent, error) {
	var moov []byte
	var extents []fileExtent
	for off, count := int64(0), 0; off < size; count++ {
		if count >= 4096 || size-off < 8 {
			return nil, nil, errors.New("mp4: invalid file box layout")
		}
		header := make([]byte, 8)
		if _, err := r.ReadAt(header, off); err != nil {
			return nil, nil, err
		}
		n := uint64(binary.BigEndian.Uint32(header))
		h := int64(8)
		if n == 1 {
			if _, err := r.ReadAt(header, off+8); err != nil {
				return nil, nil, err
			}
			n = binary.BigEndian.Uint64(header)
			h = 16
			if _, err := r.ReadAt(header, off); err != nil {
				return nil, nil, err
			}
		} else if n == 0 {
			n = uint64(size - off)
		}
		if n < uint64(h) || n > uint64(size-off) {
			return nil, nil, errors.New("mp4: box outside file")
		}
		switch string(header[4:8]) {
		case "moov":
			if moov != nil || n > 8<<20 {
				return nil, nil, errors.New("mp4: duplicate or oversized movie index")
			}
			moov = make([]byte, int(n))
			if _, err := r.ReadAt(moov, off); err != nil {
				return nil, nil, err
			}
		case "mdat":
			extents = append(extents, fileExtent{off + h, off + int64(n)})
		case "moof":
			return nil, nil, errors.New("mp4: direct fragmented MP4 input is not supported")
		}
		off += int64(n)
	}
	if moov == nil || len(extents) == 0 {
		return nil, nil, errors.New("mp4: missing movie index or media data")
	}
	return moov, extents, nil
}

func decodeMovie(data []byte) (movie *ff.MoovBox, err error) {
	// Contain parser panics from malformed sample tables; size is bounded before decoding.
	defer func() {
		if recover() != nil {
			movie = nil
			err = errors.New("mp4: malformed movie index")
		}
	}()
	index, err := sampleIndex(data)
	if err != nil {
		return nil, err
	}
	b, err := ff.DecodeBox(0, bytes.NewReader(index))
	if err != nil {
		return nil, err
	}
	movie, ok := b.(*ff.MoovBox)
	if !ok {
		return nil, errors.New("mp4: expected movie index")
	}
	return movie, nil
}

func (p *FileProducer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	for _, r := range p.Receivers {
		if r.Codec == codec {
			return r, nil
		}
	}
	r, err := p.Connection.GetTrack(media, codec)
	if err == nil {
		r.Lossless = true
	}
	return r, err
}

func (p *FileProducer) IsFinite() bool { return true }
func (p *FileProducer) Stop() error {
	p.cancel()
	for _, r := range p.Receivers {
		r.End(context.Canceled)
	}
	return p.Connection.Stop()
}
func (p *FileProducer) Start() (err error) {
	defer func() {
		if closer, ok := p.Transport.(io.Closer); ok {
			_ = closer.Close()
		}
		for _, r := range p.Receivers {
			r.End(err)
		}
	}()
	indices := make([]int, len(p.tracks))
	start := time.Now()
	for {
		selected := -1
		for i, t := range p.tracks {
			if indices[i] < len(t.samples) && (selected < 0 || t.samples[indices[i]].at < p.tracks[selected].samples[indices[selected]].at) {
				selected = i
			}
		}
		if selected < 0 {
			return nil
		}
		t := p.tracks[selected]
		s := t.samples[indices[selected]]
		indices[selected]++
		if delay := s.at - time.Since(start); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-p.ctx.Done():
				timer.Stop()
				return p.ctx.Err()
			case <-timer.C:
			}
		}
		if err = p.ctx.Err(); err != nil {
			return err
		}
		data := make([]byte, s.size)
		if _, err = p.reader.ReadAt(data, s.offset); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		minNAL := 1
		if t.config.codec.Name == core.CodecH265 {
			minNAL = 2
		}
		data, err = normalizeNALs(data, t.config.nalLength, minNAL)
		if err != nil {
			return err
		}
		packet := &core.Packet{Payload: data}
		s.timing.ClockID = p.ID
		core.SetSampleTiming(packet, s.timing)
		packet.Timestamp = uint32(s.timing.DecodeTime) + s.timing.CompositionOffset
		p.Recv += len(data)
		for _, r := range p.Receivers {
			if r.Codec == t.config.codec {
				r.WriteRTP(packet)
			}
		}
	}
}
