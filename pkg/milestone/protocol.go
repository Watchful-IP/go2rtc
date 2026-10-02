package milestone

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ImageServer framing limits. A 4K keyframe is well under 4 MiB; a GOP is a
// few hundred pictures at most.
const (
	maxXMLSize     = 1 << 20
	maxHeaderLines = 64
	maxPartSize    = 16 << 20
	maxParts       = 4096
)

var errDesync = errors.New("milestone: ImageServer stream out of sync")

// response is one ImageServer message: an XML document (methodresponse or
// livepackage) or an image response with one payload per part.
type response struct {
	xml    []byte
	header map[string]string // lower-case keys
	parts  [][]byte
}

func (r *response) isXML() bool { return r.xml != nil }

func (r *response) int64(key string) int64 {
	v, _ := strconv.ParseInt(r.header[key], 10, 64)
	return v
}

func methodCall(id int, method, params string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><methodcall><requestid>` + strconv.Itoa(id) +
		`</requestid><methodname>` + method + `</methodname>` + params + "</methodcall>\r\n\r\n")
}

type reader struct {
	rd *bufio.Reader
}

func newReader(r io.Reader) *reader {
	return &reader{rd: bufio.NewReaderSize(r, 64<<10)}
}

func (r *reader) read() (*response, error) {
	b, err := r.rd.Peek(1)
	if err != nil {
		return nil, err
	}
	switch b[0] {
	case '<':
		doc, err := r.readUntil([]byte("\r\n\r\n"), maxXMLSize)
		if err != nil {
			return nil, err
		}
		return &response{xml: doc}, nil
	case 'I':
		return r.readImage()
	}
	return nil, errDesync
}

func (r *reader) readImage() (*response, error) {
	line, err := r.readLine()
	if err != nil {
		return nil, err
	}
	if line != "ImageResponse" {
		return nil, errDesync
	}
	header, err := r.readHeader()
	if err != nil {
		return nil, err
	}
	res := &response{header: header}

	if ct := header["content-type"]; strings.HasPrefix(ct, "multipart/") {
		boundary := multipartBoundary(ct)
		if boundary == "" {
			return nil, errDesync
		}
		return res, r.readParts(res, "--"+boundary)
	}

	size, err := strconv.Atoi(header["content-length"])
	if err != nil || size < 0 || size > maxPartSize {
		return nil, errDesync
	}
	if size > 0 {
		part := make([]byte, size)
		if _, err = io.ReadFull(r.rd, part); err != nil {
			return nil, err
		}
		res.parts = [][]byte{part}
	}
	return res, r.expect("\r\n\r\n")
}

// readParts reads `--b CRLF headers CRLF CRLF body CRLF` until `--b-- CRLF CRLF`.
func (r *reader) readParts(res *response, boundary string) error {
	for {
		line, err := r.readLine()
		if err != nil {
			return err
		}
		switch line {
		case "":
			continue
		case boundary + "--":
			return r.expect("\r\n")
		case boundary:
		default:
			return errDesync
		}
		header, err := r.readHeader()
		if err != nil {
			return err
		}
		size, err := strconv.Atoi(header["content-length"])
		if err != nil || size < 0 || size > maxPartSize || len(res.parts) >= maxParts {
			return errDesync
		}
		part := make([]byte, size)
		if _, err = io.ReadFull(r.rd, part); err != nil {
			return err
		}
		res.parts = append(res.parts, part)
	}
}

func (r *reader) readHeader() (map[string]string, error) {
	header := map[string]string{}
	for i := 0; ; i++ {
		if i == maxHeaderLines {
			return nil, errDesync
		}
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		if line == "" {
			return header, nil
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			header[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
}

func (r *reader) readLine() (string, error) {
	b, err := r.readUntil([]byte("\r\n"), 8<<10)
	return string(b), err
}

// readUntil returns the bytes before sep and consumes sep.
func (r *reader) readUntil(sep []byte, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.rd.ReadSlice(sep[len(sep)-1])
		buf = append(buf, chunk...)
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
		if len(buf) > limit {
			return nil, errDesync
		}
		if bytes.HasSuffix(buf, sep) {
			return buf[:len(buf)-len(sep)], nil
		}
	}
}

func (r *reader) expect(s string) error {
	b := make([]byte, len(s))
	if _, err := io.ReadFull(r.rd, b); err != nil {
		return err
	}
	if string(b) != s {
		return errDesync
	}
	return nil
}

var reBoundary = regexp.MustCompile(`boundary="?([^";\s]+)`)

func multipartBoundary(contentType string) string {
	if m := reBoundary.FindStringSubmatch(contentType); m != nil {
		return m[1]
	}
	return ""
}

// xmlValue returns the text of the first <tag> element; good enough for the
// flat ImageServer documents.
func xmlValue(doc []byte, tag string) string {
	open := []byte("<" + tag + ">")
	i := bytes.Index(doc, open)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(open):]
	j := bytes.Index(rest, []byte("</"+tag+">"))
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

var reStatusItem = regexp.MustCompile(`<statusitem id="(\d+)" value="(\d+)"`)

// statusConnectionLost is the livepackage item for the camera-to-recorder link.
const statusConnectionLost = 5

func liveStatus(doc []byte) map[int]int {
	items := map[int]int{}
	for _, m := range reStatusItem.FindAllSubmatch(doc, -1) {
		id, _ := strconv.Atoi(string(m[1]))
		v, _ := strconv.Atoi(string(m[2]))
		items[id] = v
	}
	return items
}

func methodError(doc []byte) error {
	reason := xmlValue(doc, "errorreason")
	if reason == "" {
		reason = "rejected"
	}
	return fmt.Errorf("milestone: %s: %s", xmlValue(doc, "methodname"), reason)
}
