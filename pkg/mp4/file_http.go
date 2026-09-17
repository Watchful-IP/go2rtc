package mp4

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tcp"
)

const rangeBlock = 256 << 10
const maxSpool = 256 << 20

// fileHTTP reads bounded ranges, or spools a bounded response when ranges aren't advertised.
// Requests use the final response URL and its headers, preserving redirect credential policy.
type fileHTTP struct {
	ctx       context.Context
	cancel    context.CancelFunc
	req       *http.Request
	size      int64
	validator string
	etag      bool
	cache     []byte
	offset    int64
	file      *os.File
	closeOnce sync.Once
}

func newFileHTTP(res *http.Response) (*fileHTTP, error) {
	ctx, cancel := context.WithCancel(res.Request.Context())
	r := &fileHTTP{ctx: ctx, cancel: cancel, req: res.Request.Clone(ctx), size: res.ContentLength, offset: -1}
	defer res.Body.Close()
	r.req.Header = res.Request.Header.Clone()
	r.req.Header.Del("Range")
	r.req.Header.Del("If-Range")
	r.req.Header.Set("Accept-Encoding", "identity")
	if res.Header.Get("Content-Encoding") != "" && res.Header.Get("Content-Encoding") != "identity" {
		cancel()
		return nil, errors.New("mp4: compressed HTTP file unsupported")
	}
	if r.size > 0 && r.size <= 64<<30 && strings.EqualFold(res.Header.Get("Accept-Ranges"), "bytes") {
		r.validator = res.Header.Get("ETag")
		r.etag = r.validator != "" && !strings.HasPrefix(r.validator, "W/")
		if !r.etag {
			r.validator = res.Header.Get("Last-Modified")
		}
		return r, nil
	}
	if r.size > maxSpool {
		cancel()
		return nil, errors.New("mp4: file without byte ranges exceeds spool limit")
	}
	f, err := os.CreateTemp("", "go2rtc-mp4-*")
	if err != nil {
		cancel()
		return nil, err
	}
	r.file = f
	// The temporary file stays private and is removed on every failure and Stop.
	timer := time.AfterFunc(30*time.Second, func() { _ = res.Body.Close() })
	n, err := io.Copy(f, io.LimitReader(res.Body, maxSpool+1))
	timer.Stop()
	if err != nil || n > maxSpool || (r.size >= 0 && n != r.size) {
		r.Close()
		return nil, errors.New("mp4: incomplete or oversized HTTP file")
	}
	r.size = n
	return r, nil
}

func (r *fileHTTP) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.file != nil {
		return r.file.ReadAt(p, off)
	}
	if off < 0 || off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for len(p) > 0 && off < r.size {
		if off < r.offset || off >= r.offset+int64(len(r.cache)) {
			if err := r.fetch(off); err != nil {
				return n, err
			}
		}
		k := copy(p, r.cache[off-r.offset:])
		p = p[k:]
		off += int64(k)
		n += k
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func (r *fileHTTP) fetch(off int64) error {
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	req := r.req.Clone(ctx)
	end := min(off+rangeBlock, r.size) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	if r.validator != "" {
		req.Header.Set("If-Range", r.validator)
	}
	res, err := tcp.Do(req)
	if err != nil {
		return errors.New("mp4: HTTP range request failed")
	}
	defer res.Body.Close()
	expected := fmt.Sprintf("bytes %d-%d/%d", off, end, r.size)
	if res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Range") != expected {
		return errors.New("mp4: invalid range response or file changed")
	}
	if enc := res.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return errors.New("mp4: compressed range response")
	}
	if r.validator != "" {
		key := "Last-Modified"
		if r.etag {
			key = "ETag"
		}
		if res.Header.Get(key) != r.validator {
			return errors.New("mp4: file validator changed")
		}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, end-off+2))
	if err != nil || int64(len(data)) != end-off+1 {
		return errors.New("mp4: truncated range response")
	}
	r.cache = data
	r.offset = off
	return nil
}

func (r *fileHTTP) Close() error {
	r.cancel()
	r.closeOnce.Do(func() {
		if r.file != nil {
			_ = r.file.Close()
			_ = os.Remove(r.file.Name())
		}
	})
	return nil
}

// OpenFileResponse takes ownership of the response body.
func OpenFileResponse(res *http.Response) (p *FileProducer, err error) {
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, errors.New("mp4: HTTP status " + strconv.Itoa(res.StatusCode))
	}
	r, err := newFileHTTP(res)
	if err != nil {
		return nil, err
	}
	base := r.ctx
	openCtx, cancel := context.WithTimeout(base, 30*time.Second)
	r.ctx = openCtx
	p, err = OpenFile(r, r.size)
	r.ctx = base
	cancel()
	if err != nil {
		r.Close()
		return nil, err
	}
	p.Connection.Transport = r
	return p, nil
}
