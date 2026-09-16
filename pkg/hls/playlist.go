package hls

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type resource struct {
	uri            string
	offset, length int64
}
type segment struct {
	resource
	init                    resource
	sequence, discontinuity uint64
	duration                time.Duration
	gap                     bool
	key, initKey            encryption
}
type playlist struct {
	segments []segment
	variant  string
	target   time.Duration
	end      bool
}

func parsePlaylist(data []byte, base *url.URL) (*playlist, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, errors.New("hls: invalid playlist")
	}
	p := &playlist{target: time.Second}
	var seq, disc uint64
	var init, previous resource
	var key, initKey encryption
	var duration time.Duration
	var rangeValue string
	var variant, gap bool
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		tag, value, _ := strings.Cut(line, ":")
		var err error
		switch tag {
		case "#EXT-X-STREAM-INF":
			variant = true
		case "#EXT-X-MEDIA":
			attrs, e := attributes(value)
			if e != nil {
				return nil, e
			}
			if attrs["URI"] != "" && (attrs["TYPE"] == "AUDIO" || attrs["TYPE"] == "VIDEO") {
				return nil, errors.New("hls: separate renditions require ffmpeg")
			}
		case "#EXT-X-MEDIA-SEQUENCE":
			seq, err = strconv.ParseUint(value, 10, 64)
		case "#EXT-X-DISCONTINUITY-SEQUENCE":
			disc, err = strconv.ParseUint(value, 10, 64)
		case "#EXT-X-DISCONTINUITY":
			if disc == math.MaxUint64 {
				return nil, errors.New("hls: discontinuity overflow")
			}
			disc++
		case "#EXT-X-TARGETDURATION":
			p.target, err = seconds(value)
		case "#EXTINF":
			v, _, _ := strings.Cut(value, ",")
			duration, err = seconds(v)
		case "#EXT-X-BYTERANGE":
			rangeValue = value
		case "#EXT-X-MAP":
			attrs, e := attributes(value)
			if e != nil {
				return nil, e
			}
			init, err = resolveResource(base, attrs["URI"], attrs["BYTERANGE"], resource{})
			initKey = key
			if key.uri != "" && !key.explicitIV {
				return nil, errors.New("hls: encrypted EXT-X-MAP requires an explicit IV")
			}
		case "#EXT-X-KEY":
			attrs, e := attributes(value)
			if e != nil {
				return nil, e
			}
			key, err = parseEncryption(attrs, base)
		case "#EXT-X-I-FRAMES-ONLY", "#EXT-X-SKIP":
			return nil, fmt.Errorf("hls: %s requires ffmpeg", tag)
		case "#EXT-X-GAP":
			gap = true
		case "#EXT-X-ENDLIST":
			p.end = true
		default:
			if strings.HasPrefix(line, "#") {
				continue
			}
			if variant {
				r, e := resolveResource(base, line, "", resource{})
				if e != nil {
					return nil, e
				}
				if p.variant == "" {
					p.variant = r.uri
				}
				variant = false
				continue
			}
			if duration == 0 {
				return nil, errors.New("hls: segment without EXTINF")
			}
			r, e := resolveResource(base, line, rangeValue, previous)
			if e != nil {
				return nil, e
			}
			p.segments = append(p.segments, segment{r, init, seq, disc, duration, gap, key, initKey})
			if seq == math.MaxUint64 {
				return nil, errors.New("hls: media sequence overflow")
			}
			seq++
			previous = r
			rangeValue = ""
			duration = 0
			gap = false
		}
		if err != nil {
			return nil, fmt.Errorf("hls: invalid %s", tag)
		}
	}
	if variant || duration != 0 || rangeValue != "" {
		return nil, errors.New("hls: incomplete playlist")
	}
	if p.variant != "" && len(p.segments) > 0 {
		return nil, errors.New("hls: mixed master and media playlist")
	}
	return p, nil
}

func seconds(s string) (time.Duration, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0.001 || v > 3600 {
		return 0, errors.New("invalid duration")
	}
	return time.Duration(v * float64(time.Second)), nil
}

func resolveResource(base *url.URL, uri, span string, previous resource) (resource, error) {
	if uri == "" {
		return resource{}, errors.New("hls: missing URI")
	}
	ref, err := url.Parse(uri)
	if err != nil {
		return resource{}, errors.New("hls: invalid URI")
	}
	u := base.ResolveReference(ref)
	if u.Scheme != "http" && u.Scheme != "https" {
		return resource{}, errors.New("hls: unsupported URI scheme")
	}
	r := resource{uri: u.String()}
	if span == "" {
		return r, nil
	}
	n, off, explicit := strings.Cut(span, "@")
	r.length, err = strconv.ParseInt(n, 10, 64)
	if err != nil || r.length <= 0 {
		return r, errors.New("hls: invalid byte range")
	}
	if explicit {
		r.offset, err = strconv.ParseInt(off, 10, 64)
	} else {
		if previous.uri != r.uri || previous.length == 0 {
			return r, errors.New("hls: implicit range without preceding range")
		}
		r.offset = previous.offset + previous.length
	}
	if err != nil || r.offset < 0 || r.offset > math.MaxInt64-r.length {
		return r, errors.New("hls: invalid range offset")
	}
	return r, nil
}

func attributes(s string) (map[string]string, error) {
	out := make(map[string]string)
	for s != "" {
		key, tail, ok := strings.Cut(s, "=")
		if !ok || key == "" {
			return nil, errors.New("hls: invalid attribute list")
		}
		var value string
		if strings.HasPrefix(tail, "\"") {
			end := strings.IndexByte(tail[1:], '"')
			if end < 0 {
				return nil, errors.New("hls: unterminated attribute")
			}
			value = tail[1 : 1+end]
			s = tail[end+2:]
			if s != "" {
				if s[0] != ',' {
					return nil, errors.New("hls: invalid attribute separator")
				}
				s = s[1:]
			}
		} else {
			value, s, _ = strings.Cut(tail, ",")
		}
		if _, exists := out[key]; exists {
			return nil, errors.New("hls: duplicate attribute")
		}
		out[key] = value
	}
	return out, nil
}
