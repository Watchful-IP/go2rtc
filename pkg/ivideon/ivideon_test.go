package ivideon

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestDemuxErrorStopsPacingWorker(t *testing.T) {
	data, err := os.ReadFile("../mp4/testdata/h264.mp4")
	require.NoError(t, err)
	split := 0
	for p := 0; p+8 < len(data); {
		if string(data[p+4:p+8]) == "moof" {
			split = p
			break
		}
		p += int(binary.BigEndian.Uint32(data[p:]))
	}
	require.NotZero(t, split)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(message{Type: "stream-init", CodecString: "avc1", Data: data[:split]})
		_ = conn.WriteJSON(message{Type: "fragment"})
		_ = conn.WriteMessage(websocket.BinaryMessage, data[split:])
		_ = conn.WriteJSON(message{Type: "fragment"})
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3})
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	p := &Producer{Connection: core.Connection{Transport: conn}, conn: conn, done: make(chan struct{})}
	defer p.Stop()
	require.NoError(t, p.probe())
	require.Len(t, p.Medias, 2)
	result := make(chan error, 1)
	go func() { result <- p.Start() }()
	select {
	case err := <-result:
		require.ErrorContains(t, err, "mp4:")
	case <-time.After(time.Second):
		t.Fatal("Start blocked after malformed fragment")
	}
}
