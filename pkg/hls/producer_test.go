package hls

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

func mp4Fixture(t *testing.T, name string) ([]byte, []byte) {
	t.Helper()
	b, err := os.ReadFile("../mp4/testdata/" + name + ".mp4")
	require.NoError(t, err)
	for p := 0; p+8 < len(b); {
		if string(b[p+4:p+8]) == "moof" {
			return b[:p], b[p:]
		}
		p += int(binary.BigEndian.Uint32(b[p:]))
	}
	t.Fatal("missing moof")
	return nil, nil
}

func encrypt(data, key, iv []byte) []byte {
	n := 16 - len(data)%16
	out := append(bytes.Clone(data), bytes.Repeat([]byte{byte(n)}, n)...)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, out)
	return out
}

func TestProducerNativeCodecsAndEncryption(t *testing.T) {
	for _, name := range []string{"h264", "h265"} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/encrypted=%v", name, encrypted), func(t *testing.T) {
				t.Parallel()
				init, segment := mp4Fixture(t, name)
				key := bytes.Repeat([]byte{0x19}, 16)
				iv := make([]byte, 16)
				iv[15] = 7
				manifest := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:7\n"
				if encrypted {
					manifest += "#EXT-X-KEY:METHOD=AES-128,URI=\"key\",IV=0x7\n"
					init = encrypt(init, key, iv)
					segment = encrypt(segment, key, iv)
				}
				manifest += "#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.1,\nsegment.m4s\n#EXT-X-ENDLIST"
				var keyRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/index":
						io.WriteString(w, manifest)
					case "/init.mp4":
						w.Write(init)
					case "/segment.m4s":
						w.Write(segment)
					case "/key":
						keyRequests.Add(1)
						w.Write(key)
					default:
						w.WriteHeader(404)
					}
				}))
				defer server.Close()
				res, err := http.Get(server.URL + "/index")
				require.NoError(t, err)
				prod, err := OpenResponse(res)
				require.NoError(t, err)
				defer prod.Stop()
				require.Equal(t, "hls/fmp4", prod.(*Producer).FormatName)
				var count int
				for _, media := range prod.GetMedias() {
					r, err := prod.GetTrack(media, media.Codecs[0])
					require.NoError(t, err)
					r.Input = func(packet *core.Packet) { count++; require.NotEmpty(t, packet.Payload) }
				}
				start := time.Now()
				require.ErrorIs(t, prod.Start(), io.EOF)
				require.GreaterOrEqual(t, count, 60)
				require.Greater(t, time.Since(start), 900*time.Millisecond)
				if encrypted {
					require.Equal(t, int32(2), keyRequests.Load())
				}
			})
		}
	}
}

func TestAESSequenceIVAndRotation(t *testing.T) {
	keys := [][]byte{bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:42\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:1,\nfirst\n#EXTINF:1,\nsecond\n#EXT-X-ENDLIST")
			return
		}
		if r.URL.Path == "/key" {
			w.Write(keys[calls.Add(1)-1])
			return
		}
		idx := 0
		if r.URL.Path == "/second" {
			idx = 1
		}
		iv := make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], uint64(42+idx))
		w.Write(encrypt([]byte("packet"), keys[idx], iv))
	}))
	defer server.Close()
	rd := openReader(t, server.URL+"/index")
	b, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "packetpacket", string(b))
	require.Equal(t, int32(2), calls.Load())
}

func TestAESRejectsInvalidCiphertext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 16)) }))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL, nil)
	rd, err := newReader(req, io.NopCloser(bytes.NewBufferString("#EXTM3U\n#EXT-X-ENDLIST")))
	require.NoError(t, err)
	defer rd.Close()
	for _, b := range [][]byte{{1}, make([]byte, 16)} {
		_, err = rd.decrypt(b, encryption{uri: server.URL}, 0)
		require.Error(t, err)
	}
}

func TestProducerStopInterruptsPacing(t *testing.T) {
	init, segment := mp4Fixture(t, "h264")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/init":
			w.Write(init)
		case "/seg":
			w.Write(segment)
		default:
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:1.1,\nseg\n#EXT-X-ENDLIST")
		}
	}))
	defer server.Close()
	res, err := http.Get(server.URL + "/index")
	require.NoError(t, err)
	p, err := OpenResponse(res)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- p.Start() }()
	require.NoError(t, p.Stop())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Stop did not interrupt pacing")
	}
}

func TestProducerDiscontinuityAndMapRefresh(t *testing.T) {
	init, segment := mp4Fixture(t, "h264")
	var inits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/init":
			inits.Add(1)
			w.Write(init)
		case "/seg":
			w.Write(segment)
		default:
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:1.1,\nseg\n#EXT-X-DISCONTINUITY\n#EXTINF:1.1,\nseg\n#EXT-X-ENDLIST")
		}
	}))
	defer server.Close()
	res, err := http.Get(server.URL + "/index")
	require.NoError(t, err)
	p, err := OpenResponse(res)
	require.NoError(t, err)
	defer p.Stop()
	media := p.GetMedias()[1]
	r, err := p.GetTrack(media, media.Codecs[0])
	require.NoError(t, err)
	count := 0
	var last uint32
	r.Input = func(packet *core.Packet) {
		if count > 0 {
			require.Greater(t, packet.Timestamp, last)
		}
		last = packet.Timestamp
		count++
	}
	require.ErrorIs(t, p.Start(), io.EOF)
	require.Equal(t, int32(2), inits.Load())
	require.Greater(t, count, 100)
}

func TestMPEGTSRegression(t *testing.T) {
	data, err := os.ReadFile("../mp4/testdata/h264.ts")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/segment.ts" {
			w.Write(data)
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXTINF:1.1,\nsegment.ts\n#EXT-X-ENDLIST")
	}))
	defer server.Close()
	res, err := http.Get(server.URL + "/index")
	require.NoError(t, err)
	p, err := OpenResponse(res)
	require.NoError(t, err)
	defer p.Stop()
	require.Len(t, p.GetMedias(), 2)
	count := 0
	for _, m := range p.GetMedias() {
		r, err := p.GetTrack(m, m.Codecs[0])
		require.NoError(t, err)
		r.Input = func(packet *core.Packet) { count++ }
	}
	require.ErrorIs(t, p.Start(), io.EOF)
	require.Greater(t, count, 10)
}

func TestProducerPrefetchOverlapsPlayback(t *testing.T) {
	init, segment := mp4Fixture(t, "h264")
	prefetched := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/init":
			w.Write(init)
		case "/first":
			w.Write(segment)
		case "/second":
			close(prefetched)
			w.Write(segment)
		default:
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:1.1,\nfirst\n#EXT-X-DISCONTINUITY\n#EXTINF:1.1,\nsecond\n#EXT-X-ENDLIST")
		}
	}))
	defer server.Close()
	res, err := http.Get(server.URL + "/index")
	require.NoError(t, err)
	p, err := OpenResponse(res)
	require.NoError(t, err)
	defer p.Stop()
	done := make(chan error, 1)
	go func() { done <- p.Start() }()
	select {
	case <-prefetched:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("next download waited for playback")
	}
	p.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prefetch did not stop")
	}
}
