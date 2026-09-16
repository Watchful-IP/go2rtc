package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

const maxPlaylistSize = 2 << 20
const maxSegmentSize = 64 << 20
const maxInitSize = 8 << 20

type reader struct {
	client   *http.Client
	request  *http.Request
	ctx      context.Context
	cancel   context.CancelFunc
	playlist *playlist
	next     uint64
	started  bool
	lastTime time.Time
	buf      []byte
	init     resource
	initKey  encryption
	initData []byte
}

// NewReader retains the byte-stream API for MPEG-TS callers.
func NewReader(u *url.URL, body io.ReadCloser) (io.Reader, error) {
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		body.Close()
		return nil, err
	}
	return newReader(req, body)
}

func newReader(req *http.Request, body io.ReadCloser) (*reader, error) {
	defer body.Close()
	data, err := readLimited(body, maxPlaylistSize)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	r := &reader{client: &http.Client{Timeout: core.ConnDialTimeout, CheckRedirect: checkRedirect}, request: req.Clone(ctx), ctx: ctx, cancel: cancel, lastTime: time.Now()}
	for depth := 0; depth < 5; depth++ {
		p, err := parsePlaylist(data, r.request.URL)
		if err != nil {
			cancel()
			return nil, err
		}
		if p.variant == "" {
			r.playlist = p
			if !p.end && len(p.segments) > 0 {
				duration := time.Duration(0)
				i := len(p.segments)
				for i > 0 && duration < 3*p.target {
					i--
					duration += p.segments[i].duration
				}
				r.playlist.segments = p.segments[i:]
			}
			return r, nil
		}
		var u *url.URL
		data, u, err = r.fetch(resource{uri: p.variant}, maxPlaylistSize)
		if err != nil {
			cancel()
			return nil, err
		}
		r.request = r.childRequest(u)
	}
	cancel()
	return nil, errors.New("hls: too many nested playlists")
}

func (r *reader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if len(r.buf) == 0 {
		seg, data, err := r.nextSegment()
		if err != nil {
			return 0, err
		}
		if seg.init.uri != "" {
			return 0, errors.New("hls: fMP4 requires segment reader")
		}
		r.buf = data
	}
	n := copy(dst, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
func (r *reader) Close() error { r.cancel(); return nil }

func (r *reader) nextSegment() (segment, []byte, error) {
	stalled := time.Now()
	for {
		if err := r.ctx.Err(); err != nil {
			return segment{}, nil, err
		}
		for _, s := range r.playlist.segments {
			if r.started && s.sequence < r.next {
				continue
			}
			if r.started && s.sequence > r.next {
				return segment{}, nil, errors.New("hls: live window skipped segments; reconnect required")
			}
			if s.gap {
				return segment{}, nil, errors.New("hls: segment gap; reconnect required")
			}
			data, _, err := r.fetch(s.resource, maxSegmentSize)
			if err != nil {
				return segment{}, nil, err
			}
			data, err = r.decrypt(data, s.key, s.sequence)
			if err != nil {
				return segment{}, nil, err
			}
			if len(data) == 0 {
				return segment{}, nil, errors.New("hls: empty segment")
			}
			r.next = s.sequence + 1
			r.started = true
			return s, data, nil
		}
		if r.playlist.end {
			return segment{}, nil, io.EOF
		}
		if time.Since(stalled) > max(30*time.Second, 3*r.playlist.target) {
			return segment{}, nil, errors.New("hls: playlist stalled")
		}
		wait := r.playlist.target/2 - time.Since(r.lastTime)
		if err := r.wait(wait); err != nil {
			return segment{}, nil, err
		}
		data, u, err := r.fetch(resource{uri: r.request.URL.String()}, maxPlaylistSize)
		if err != nil {
			return segment{}, nil, err
		}
		p, err := parsePlaylist(data, u)
		if err != nil {
			return segment{}, nil, err
		}
		if p.variant != "" {
			return segment{}, nil, errors.New("hls: media playlist became master")
		}
		r.request = r.childRequest(u)
		r.playlist = p
		r.lastTime = time.Now()
	}
}

func (r *reader) initialization(s segment) ([]byte, error) {
	if s.init.uri == "" {
		return nil, errors.New("hls: missing EXT-X-MAP")
	}
	if r.init != s.init || r.initKey != s.initKey {
		data, _, err := r.fetch(s.init, maxInitSize)
		if err != nil {
			return nil, err
		}
		data, err = r.decrypt(data, s.initKey, 0)
		if err != nil {
			return nil, err
		}
		r.init = s.init
		r.initKey = s.initKey
		r.initData = data
	}
	return r.initData, nil
}

func (r *reader) childRequest(u *url.URL) *http.Request {
	req := r.request.Clone(r.ctx)
	req.URL = u
	// Credentials belong to an origin, not to every URI mentioned in a playlist.
	if u.Scheme != r.request.URL.Scheme || u.Host != r.request.URL.Host {
		req.Header = make(http.Header)
	}
	req.Header.Del("Range")
	return req
}

func (r *reader) fetch(res resource, limit int64) ([]byte, *url.URL, error) {
	u, err := url.Parse(res.uri)
	if err != nil {
		return nil, nil, errors.New("hls: invalid URL")
	}
	if res.length > limit {
		return nil, nil, errors.New("hls: resource exceeds size limit")
	}
	for attempt := 0; attempt < 3; attempt++ {
		req := r.childRequest(u)
		if res.length != 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", res.offset, res.offset+res.length-1))
			req.Header.Set("Accept-Encoding", "identity")
		}
		response, err := r.client.Do(req)
		if err != nil {
			if r.ctx.Err() != nil {
				return nil, nil, r.ctx.Err()
			}
			if attempt < 2 {
				if err = r.wait(time.Duration(attempt+1) * 200 * time.Millisecond); err != nil {
					return nil, nil, err
				}
				continue
			}
			// net/url errors include the full URL, which can contain signed credentials.
			return nil, nil, errors.New("hls: HTTP request failed")
		}
		status := response.StatusCode
		if (status == 429 || status >= 500) && attempt < 2 {
			response.Body.Close()
			if err = r.wait(time.Duration(attempt+1) * 200 * time.Millisecond); err != nil {
				return nil, nil, err
			}
			continue
		}
		if res.length != 0 {
			var start, end, total int64
			count, _ := fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
			if status != http.StatusPartialContent || count != 3 || start != res.offset || end != res.offset+res.length-1 || total <= end {
				response.Body.Close()
				return nil, nil, errors.New("hls: invalid byte-range response")
			}
			limit = res.length
		} else if status != http.StatusOK {
			response.Body.Close()
			return nil, nil, fmt.Errorf("hls: HTTP status %d", status)
		}
		data, err := readLimited(response.Body, limit)
		response.Body.Close()
		if r.ctx.Err() != nil {
			return nil, nil, r.ctx.Err()
		}
		if err != nil {
			return nil, nil, err
		}
		if res.length != 0 && int64(len(data)) != res.length {
			return nil, nil, errors.New("hls: truncated range response")
		}
		return data, response.Request.URL, nil
	}
	return nil, nil, errors.New("hls: HTTP retries exhausted")
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("hls: response body read failed")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("hls: resource exceeds size limit")
	}
	return data, nil
}

func (r *reader) wait(d time.Duration) error {
	if d <= 0 {
		return r.ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("hls: too many redirects")
	}
	previous := via[len(via)-1].URL
	if req.URL.Scheme != previous.Scheme || req.URL.Host != previous.Host {
		headers := make(http.Header)
		for _, name := range []string{"Range", "Accept-Encoding"} {
			if value := req.Header.Get(name); value != "" {
				headers.Set(name, value)
			}
		}
		req.Header = headers
	}
	return nil
}
