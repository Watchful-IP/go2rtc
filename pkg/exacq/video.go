package exacq

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/reorder"
)

type videoFrame struct {
	time time.Time // recorder stamp; the output decode time once timed
	data []byte    // AVCC, including retained parameter sets on keyframes
	key  bool
	pic  reorder.Picture
	// backdated pictures came from the recorder's untimed live buffer.
	backdated bool
	// Set by the timeline: presentation is zero when it equals time.
	presentation time.Time
	duration     time.Duration
}

type videoState struct {
	format   uint32
	params   [3][]byte // H264 SPS/PPS or H265 VPS/SPS/PPS
	ready    bool
	pictures reorder.Parser
	// keepUntimed returns the recorder's buffered GOP prefix (pictures with a
	// zero timestamp) so live output can start before the next keyframe.
	keepUntimed bool
}

func (v *videoState) prepare(p *MediaPacket) (*videoFrame, error) {
	if p.Type == 0 { // Audio has its own format and timestamp contract.
		return nil, nil
	}
	if p.Type != 1 && p.Type != 2 && p.Type != 3 {
		return nil, fmt.Errorf("exacq: unsupported video frame type %d", p.Type)
	}
	nalus, err := splitAnnexB(p.Data)
	if err != nil {
		return nil, err
	}
	var body []byte
	var key, picture bool
	for _, nalu := range nalus {
		if nalu[0]&0x80 != 0 {
			return nil, errors.New("exacq: invalid video NAL header")
		}
		parameter := -1
		if v.format == 5 {
			switch nalu[0] & 31 {
			case 7:
				if len(nalu) < 4 {
					return nil, errors.New("exacq: truncated H264 SPS")
				}
				parameter = 0
			case 8:
				parameter = 1
			case 9:
				continue
			case 5:
				key, picture = true, true
			case 1:
				picture = true
			case 6, 10, 11, 12:
				// SEI, end markers and filler carry no separate picture.
			default:
				return nil, errors.New("exacq: unsupported H264 NAL unit or changed codec")
			}
		} else if v.format == 6 {
			if len(nalu) < 2 || nalu[1]&7 == 0 {
				return nil, errors.New("exacq: invalid H265 NAL header")
			}
			n := (nalu[0] >> 1) & 63
			switch {
			case n >= 32 && n <= 34:
				parameter = int(n - 32)
			case n == 35:
				continue
			case n >= 16 && n <= 21:
				key, picture = true, true
			case n <= 31:
				picture = true
			case n > 40:
				return nil, errors.New("exacq: unsupported H265 NAL unit or changed codec")
			}
		} else {
			return nil, fmt.Errorf("exacq: unsupported video format %d", v.format)
		}
		if parameter >= 0 {
			if len(nalu) > 65536 {
				return nil, errors.New("exacq: video parameter set exceeds size limit")
			}
			if v.ready && !bytes.Equal(v.params[parameter], nalu) {
				return nil, errors.New("exacq: video parameters changed; reconnect required")
			}
			v.params[parameter] = bytes.Clone(nalu)
			continue
		}
		body = appendNALU(body, nalu)
	}
	stamp := p.Timestamp()
	if !picture || (stamp.IsZero() && !v.keepUntimed) {
		// Live starts with the current GOP so far, untimed. Its parameter sets
		// are always kept; its pictures only when the caller can time them.
		return nil, nil
	}
	if !v.ready && !key {
		return nil, nil
	}
	if key {
		if len(v.params[0]) == 0 || len(v.params[1]) == 0 || (v.format == 6 && len(v.params[2]) == 0) {
			return nil, nil // Wait for a decodable keyframe and parameter sets.
		}
		var avcc []byte
		for _, param := range v.params {
			if param != nil {
				avcc = appendNALU(avcc, param)
			}
		}
		body = append(avcc, body...)
	}
	v.pictures.H265 = v.format == 6
	v.ready = true
	return &videoFrame{time: stamp, data: body, key: key, pic: v.pictures.Parse(body)}, nil
}

func (v *videoState) codec(data []byte) *core.Codec {
	if v.format == 5 {
		return h264.AVCCToCodec(data)
	}
	return h265.AVCCToCodec(data)
}

func appendNALU(avcc, nalu []byte) []byte {
	avcc = binary.BigEndian.AppendUint32(avcc, uint32(len(nalu)))
	return append(avcc, nalu...)
}

// splitAnnexB accepts both standard start-code lengths, with no unframed
// prefix. Returned NAL units alias the input, and empty units are rejected.
func splitAnnexB(data []byte) ([][]byte, error) {
	var nalus [][]byte
	start := -1
	for i := 0; i+2 < len(data); {
		if data[i] != 0 || data[i+1] != 0 {
			i++
			continue
		}
		n := 0
		if data[i+2] == 1 {
			n = 3
		} else if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			n = 4
		}
		if n == 0 {
			i++
			continue
		}
		if start < 0 {
			if len(bytes.Trim(data[:i], "\x00")) != 0 {
				return nil, errors.New("exacq: invalid Annex B prefix")
			}
		} else {
			nalu := bytes.TrimRight(data[start:i], "\x00")
			if len(nalu) == 0 {
				return nil, errors.New("exacq: empty video NAL unit")
			}
			nalus = append(nalus, nalu)
		}
		start = i + n
		i = start
	}
	if start < 0 || start >= len(data) {
		return nil, errors.New("exacq: missing Annex B video NAL unit")
	}
	nalu := bytes.TrimRight(data[start:], "\x00")
	if len(nalu) == 0 {
		return nil, errors.New("exacq: empty video NAL unit")
	}
	nalus = append(nalus, nalu)
	return nalus, nil
}
