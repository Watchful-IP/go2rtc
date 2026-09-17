package mp4

import (
	"bytes"
	"io"
	"testing"

	ff "github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/require"
)

func changeIndex(t *testing.T, edit func(*ff.MoovBox)) []byte {
	t.Helper()
	data := fixture(t, "progressive")
	bs, err := boxes(data, 0, len(data))
	require.NoError(t, err)
	for _, b := range bs {
		if b.name == "moov" {
			parsed, err := ff.DecodeBox(uint64(b.start), bytes.NewReader(data[b.start:b.end]))
			require.NoError(t, err)
			movie := parsed.(*ff.MoovBox)
			edit(movie)
			out := bytes.NewBuffer(bytes.Clone(data[:b.start]))
			require.NoError(t, movie.Encode(out))
			return out.Bytes()
		}
	}
	t.Fatal("no moov")
	return nil
}

func TestProgressiveRejectsInvalidTables(t *testing.T) {
	cases := map[string]func(*ff.MoovBox){
		"sample outside mdat": func(m *ff.MoovBox) { m.Traks[0].Mdia.Minf.Stbl.Stco.ChunkOffset[0] = 0 },
		"short stts":          func(m *ff.MoovBox) { m.Traks[0].Mdia.Minf.Stbl.Stts.SampleCount[0]-- },
		"zero duration":       func(m *ff.MoovBox) { m.Traks[0].Mdia.Minf.Stbl.Stts.SampleTimeDelta[0] = 0 },
		"chunk count":         func(m *ff.MoovBox) { m.Traks[0].Mdia.Minf.Stbl.Stsc.Entries[0].SamplesPerChunk = 1000 },
		"description change":  func(m *ff.MoovBox) { m.Traks[0].Mdia.Minf.Stbl.Stsc.SetSingleSampleDescriptionID(2) },
		"negative cts":        func(m *ff.MoovBox) { c := m.Traks[0].Mdia.Minf.Stbl.Ctts; c.Version = 1; c.SampleOffset[0] = -1 },
		"missing keyframe":    func(m *ff.MoovBox) { m.Traks[0].Mdia.Minf.Stbl.Stss.SampleNumber[0] = 2 },
		"trim edit": func(m *ff.MoovBox) {
			e := &ff.EdtsBox{}
			e.AddChild(&ff.ElstBox{Entries: []ff.ElstEntry{{MediaTime: 100, SegmentDuration: 1000, MediaRateInteger: 1}}})
			m.Traks[0].AddChild(e)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			data := changeIndex(t, edit)
			_, err := OpenFile(bytes.NewReader(data), int64(len(data)))
			require.Error(t, err)
		})
	}
}

func TestProgressiveCo64(t *testing.T) {
	data := changeIndex(t, func(m *ff.MoovBox) {
		for _, tr := range m.Traks {
			st := tr.Mdia.Minf.Stbl
			co := &ff.Co64Box{}
			for _, o := range st.Stco.ChunkOffset {
				co.ChunkOffset = append(co.ChunkOffset, uint64(o))
			}
			for i, c := range st.Children {
				if c.Type() == "stco" {
					st.Children[i] = co
				}
			}
			st.Stco = nil
			st.Co64 = co
		}
	})
	p, err := OpenFile(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	defer p.Stop()
	require.Len(t, p.tracks, 2)
}

func TestProgressiveFragmentedRejected(t *testing.T) {
	data := fixture(t, "h264")
	_, err := OpenFile(bytes.NewReader(data), int64(len(data)))
	require.ErrorContains(t, err, "fragmented")
}

func TestProgressiveTruncatedPayloadFails(t *testing.T) {
	data := fixture(t, "progressive")
	p, err := OpenFile(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	defer p.Stop()
	// Metadata was complete, but the backing file disappears before playback.
	p.reader = bytes.NewReader(nil)
	require.ErrorIs(t, p.Start(), io.ErrUnexpectedEOF)
}
