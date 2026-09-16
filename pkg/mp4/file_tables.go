package mp4

import (
	"errors"
	"math"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	ff "github.com/Eyevinn/mp4ff/mp4"
)

func fileTracks(data []byte, dem *Demuxer, extents []fileExtent) (tracks []*fileTrack, err error) {
	defer func() {
		if recover() != nil {
			tracks = nil
			err = errors.New("mp4: malformed sample tables")
		}
	}()
	movie, err := decodeMovie(data)
	if err != nil {
		return nil, err
	}
	if movie.Mvex != nil {
		return nil, errors.New("mp4: fragmented movie requires a different input")
	}
	if movie.Mvhd == nil || movie.Mvhd.Timescale == 0 {
		return nil, errInvalidMP4
	}
	total := 0
	for _, trak := range movie.Traks {
		if trak.Tkhd == nil {
			return nil, errInvalidMP4
		}
		conf := dem.tracks[trak.Tkhd.TrackID]
		if conf == nil || conf.codec == nil {
			continue
		}
		if trak.Mdia == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			return nil, errInvalidMP4
		}
		samples, err := tableSamples(trak.Mdia.Minf.Stbl, extents)
		if err != nil {
			return nil, err
		}
		total += len(samples)
		if total > maxFileSamples {
			return nil, errors.New("mp4: too many file samples")
		}
		lead, err := editLead(trak, movie.Mvhd.Timescale, samples, conf.scale)
		if err != nil {
			return nil, err
		}
		for i := range samples {
			s := &samples[i]
			d := s.timing.DecodeTime
			// Source-clock endpoint conversion avoids cumulative per-sample rounding.
			decode, ok := rescaleTime(d, conf.scale, conf.codec.ClockRate)
			end, okEnd := rescaleTime(d+uint64(s.timing.Duration), conf.scale, conf.codec.ClockRate)
			cts, okCTS := rescaleTime(uint64(s.timing.CompositionOffset), conf.scale, conf.codec.ClockRate)
			if !ok || !okEnd || !okCTS || end <= decode || end-decode > math.MaxUint32 || cts > math.MaxUint32 {
				return nil, errors.New("mp4: unrepresentable sample timing")
			}
			s.at = time.Duration(d/uint64(conf.scale))*time.Second + time.Duration(d%uint64(conf.scale))*time.Second/time.Duration(conf.scale) + lead
			shift := uint64(lead/time.Second)*uint64(conf.codec.ClockRate) + uint64(lead%time.Second)*uint64(conf.codec.ClockRate)/uint64(time.Second)
			s.timing = core.SampleTiming{DecodeTime: decode + shift, Duration: uint32(end - decode), CompositionOffset: uint32(cts)}
		}
		tracks = append(tracks, &fileTrack{conf, samples})
	}
	if len(tracks) == 0 {
		return nil, errors.New("mp4: no supported file tracks")
	}
	return tracks, nil
}

// mp4ff's segmenter is the reference for stsc/stco/stsz sample extraction.
// Validate cross-table counts first, then walk each run once (not once per sample).
func tableSamples(st *ff.StblBox, extents []fileExtent) ([]fileSample, error) {
	if st.Stsz == nil || st.Stts == nil || st.Stsc == nil {
		return nil, errors.New("mp4: missing progressive sample tables")
	}
	n := st.Stsz.GetNrSamples()
	if n == 0 || n > maxFileSamples {
		return nil, errors.New("mp4: invalid sample count")
	}
	if st.Saio != nil || st.Saiz != nil {
		return nil, errors.New("mp4: encrypted file samples unsupported")
	}
	samples := make([]fileSample, n)
	for i := range samples {
		size := st.Stsz.GetSampleSize(i + 1)
		if size == 0 || size > maxFileSampleSize {
			return nil, errors.New("mp4: invalid file sample size")
		}
		samples[i].size = size
	}
	var count uint64
	var dts uint64
	for i, num := range st.Stts.SampleCount {
		dur := st.Stts.SampleTimeDelta[i]
		if num == 0 || dur == 0 || count+uint64(num) > uint64(n) {
			return nil, errors.New("mp4: invalid stts sample count or duration")
		}
		for j := uint32(0); j < num; j++ {
			samples[count].timing.DecodeTime = dts
			samples[count].timing.Duration = dur
			dts += uint64(dur)
			count++
		}
	}
	if count != uint64(n) {
		return nil, errors.New("mp4: stts sample count mismatch")
	}
	if c := st.Ctts; c != nil {
		if len(c.EndSampleNr) != len(c.SampleOffset)+1 || c.EndSampleNr[0] != 0 || c.EndSampleNr[len(c.EndSampleNr)-1] != n {
			return nil, errors.New("mp4: ctts sample count mismatch")
		}
		for i, offset := range c.SampleOffset {
			if offset < 0 {
				return nil, errors.New("mp4: negative composition offsets unsupported")
			}
			a, b := c.EndSampleNr[i], c.EndSampleNr[i+1]
			if b <= a || b > n {
				return nil, errors.New("mp4: invalid ctts run")
			}
			for j := a; j < b; j++ {
				samples[j].timing.CompositionOffset = uint32(offset)
			}
		}
	}
	var chunks []uint64
	if st.Stco != nil && st.Co64 != nil {
		return nil, errors.New("mp4: conflicting chunk offsets")
	}
	if st.Stco != nil {
		for _, o := range st.Stco.ChunkOffset {
			chunks = append(chunks, uint64(o))
		}
	} else if st.Co64 != nil {
		chunks = st.Co64.ChunkOffset
	}
	entries := st.Stsc.Entries
	if len(chunks) == 0 || len(entries) == 0 || entries[0].FirstChunk != 1 {
		return nil, errors.New("mp4: missing chunk mapping")
	}
	for i, e := range entries {
		if e.SamplesPerChunk == 0 || e.FirstChunk > uint32(len(chunks)) || (i > 0 && e.FirstChunk <= entries[i-1].FirstChunk) || st.Stsc.GetSampleDescriptionID(i+1) != 1 {
			return nil, errors.New("mp4: invalid chunk mapping or sample description")
		}
	}
	index, run := 0, 0
	for i, offset := range chunks {
		if run+1 < len(entries) && entries[run+1].FirstChunk == uint32(i+1) {
			run++
		}
		num := entries[run].SamplesPerChunk
		if uint64(index)+uint64(num) > uint64(n) {
			return nil, errors.New("mp4: chunk sample count mismatch")
		}
		for j := uint32(0); j < num; j++ {
			size := uint64(samples[index].size)
			valid := false
			for _, e := range extents {
				if offset >= uint64(e.start) && offset <= uint64(e.end) && size <= uint64(e.end)-offset {
					valid = true
					break
				}
			}
			if !valid {
				return nil, errors.New("mp4: file sample outside mdat")
			}
			samples[index].offset = int64(offset)
			offset += size
			index++
		}
	}
	if index != int(n) {
		return nil, errors.New("mp4: chunk sample count mismatch")
	}
	if st.Stss != nil {
		last := uint32(0)
		for _, s := range st.Stss.SampleNumber {
			if s <= last || s > n {
				return nil, errors.New("mp4: invalid sync sample table")
			}
			last = s
		}
		if len(st.Stss.SampleNumber) == 0 || st.Stss.SampleNumber[0] != 1 {
			return nil, errors.New("mp4: file does not start with a sync sample")
		}
	}
	return samples, nil
}

// The sample graph has no edit-list contract. Accept identity edits and empty
// lead-ins; reject trims/rate changes instead of recording a different timeline.
func editLead(trak *ff.TrakBox, movieScale uint32, samples []fileSample, scale uint32) (time.Duration, error) {
	last := samples[len(samples)-1]
	end := last.timing.DecodeTime + uint64(last.timing.Duration)
	if end/uint64(scale) > 24*60*60 {
		return 0, errors.New("mp4: file duration exceeds 24 hours")
	}
	if trak.Edts == nil || trak.Edts.Elst == nil {
		return 0, nil
	}
	if len(trak.Edts.Elst) != 1 {
		return 0, errors.New("mp4: multiple edit lists unsupported")
	}
	es := trak.Edts.Elst[0].Entries
	lead := uint64(0)
	if len(es) == 2 && es[0].MediaTime == -1 && es[0].MediaRateInteger == 1 && es[0].MediaRateFraction == 0 {
		lead = es[0].SegmentDuration
		es = es[1:]
	}
	if len(es) != 1 || es[0].MediaTime != 0 || es[0].MediaRateInteger != 1 || es[0].MediaRateFraction != 0 {
		return 0, errors.New("mp4: trimming or rate-changing edit list unsupported")
	}
	expected, ok := rescaleTime(end, scale, movieScale)
	actual := es[0].SegmentDuration
	if !ok || actual < expected || actual-expected > 1 {
		return 0, errors.New("mp4: edit duration does not cover the full track")
	}
	if lead/uint64(movieScale) > 24*60*60 {
		return 0, errors.New("mp4: edit lead-in exceeds 24 hours")
	}
	return time.Duration(lead/uint64(movieScale))*time.Second + time.Duration(lead%uint64(movieScale))*time.Second/time.Duration(movieScale), nil
}
