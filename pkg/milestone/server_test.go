package milestone

import (
	"bufio"
	"bytes"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const testCamera = "5193ca41-6223-4186-809d-668a6edc9da7"

// fakeRecorder replays ImageServer responses captured from an XProtect
// 2025 R3 recording server (testdata/*.bin, synthetic clock footage).
type fakeRecorder struct {
	t  *testing.T
	ln net.Listener

	connect  string   // connect reply; empty accepts
	live     []byte   // written after live
	gops     [][]byte // goto, then one per next; the last repeats (end of database)
	jpegLive []byte   // used when the client asks for recorder-side JPEG
	jpegGOPs [][]byte
	holdOpen bool // keep the live connection open after the fixture

	mu       sync.Mutex
	requests []string // "method" or "method jpeg"
}

func newFakeRecorder(t *testing.T) *fakeRecorder {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeRecorder{t: t, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeRecorder) url(query string) string {
	return "milestone://" + f.ln.Addr().String() + "/" + testCamera + "?token=" + url.QueryEscape("TOKEN#x#host//ServerConnector#") + query
}

func (f *fakeRecorder) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeRecorder) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRecorder) handle(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	var jpeg bool
	gop := 0
	for {
		req, err := readRequest(rd)
		if err != nil {
			return
		}
		method := xmlValue(req, "methodname")
		if method == "connect" {
			jpeg = xmlValue(req, "alwaysstdjpeg") == "yes"
		}
		f.mu.Lock()
		if jpeg {
			f.requests = append(f.requests, method+" jpeg")
		} else {
			f.requests = append(f.requests, method)
		}
		f.mu.Unlock()

		switch method {
		case "connect":
			reply := f.connect
			if reply == "" {
				reply = `<?xml version="1.0" encoding="UTF-8"?><methodresponse><requestid>1</requestid><methodname>connect</methodname><connected>yes</connected><errorreason>Success</errorreason></methodresponse>` + "\r\n\r\n"
			}
			_, _ = conn.Write([]byte(reply))
		case "live":
			live := f.live
			if jpeg {
				live = f.jpegLive
			}
			_, _ = conn.Write(live)
			if !f.holdOpen {
				return
			}
		case "goto", "next":
			gops := f.gops
			if jpeg {
				gops = f.jpegGOPs
			}
			if method == "goto" {
				gop = 0
			}
			if len(gops) == 0 {
				_, _ = conn.Write([]byte("ImageResponse\r\nRequestId: 1\r\nPrev: 0\r\nCurrent: 0\r\nNext: 0\r\nContent-length: 0\r\n\r\n\r\n\r\n"))
				continue
			}
			_, _ = conn.Write(gops[min(gop, len(gops)-1)])
			gop++
		}
	}
}

func readRequest(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	for !bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		buf = append(buf, line...)
	}
	return buf, nil
}

func fixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return b
}

func fixtures(t *testing.T, prefix string, n int) [][]byte {
	var out [][]byte
	for i := 0; i < n; i++ {
		out = append(out, fixture(t, prefix+"_gop"+string(rune('0'+i))+".bin"))
	}
	return out
}

// statusLost is a livepackage reporting the camera disconnected from the recorder.
const statusLost = `<?xml version="1.0" encoding="utf-8"?><livepackage><status><statustime>1</statustime><statusitem id="1" value="1" /><statusitem id="5" value="1" /></status></livepackage>` + "\r\n\r\n"

func hasMethod(methods []string, prefix string) bool {
	for _, m := range methods {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}
