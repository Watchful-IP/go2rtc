package rtsp

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestH265RecoverySourceOption(t *testing.T) {
	for _, mode := range []string{"", "default", "conservative"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = ln.Close() })
			done := make(chan error, 1)
			go func() {
				for i := 0; i < 2; i++ {
					conn, err := ln.Accept()
					if err != nil {
						done <- err
						return
					}
					err = serveRecoverySDP(conn)
					_ = conn.Close()
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			uri := "rtsp://" + ln.Addr().String() + "/camera?channel=1"
			if mode != "" {
				uri += "#h265_recovery=" + mode + "#timeout=5"
			}
			producer, err := rtspHandler(uri)
			require.NoError(t, err)
			t.Cleanup(func() { _ = producer.Stop() })
			conn := producer.(*rtsp.Conn)
			// Reconnection preserves the codec objects held by existing consumers.
			for i := 0; i < 2; i++ {
				var video *core.Codec
				for _, media := range conn.GetMedias() {
					for _, codec := range media.Codecs {
						want := mode == "conservative" && codec.Name == core.CodecH265
						require.Equal(t, want, codec.H265ConservativeRecovery)
						if codec.Name == core.CodecH265 {
							video = codec.Clone()
						}
					}
				}
				require.NotNil(t, video)
				calls := 0
				depay := h265.RTPDepay(video, func(*rtp.Packet) { calls++ })
				depay(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1, Timestamp: 3000, Marker: true}, Payload: []byte{2, 1, 0x80}})
				if mode == "conservative" {
					require.Zero(t, calls)
				} else {
					require.Equal(t, 1, calls)
				}
				if i == 0 {
					require.NoError(t, conn.Reconnect())
				}
			}
			require.NoError(t, <-done)
		})
	}
}

func serveRecoverySDP(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	server := rtsp.NewServer(conn)
	req, err := server.ReadRequest()
	if err != nil {
		return err
	}
	if req.Method != rtsp.MethodDescribe || req.URL.RequestURI() != "/camera?channel=1" || req.URL.Fragment != "" {
		return fmt.Errorf("unexpected camera request: %s %s", req.Method, req.URL)
	}
	body := []byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=test\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:video\r\nm=video 0 RTP/AVP 97\r\na=rtpmap:97 H264/90000\r\na=control:alternate\r\nm=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=control:audio\r\n")
	return server.WriteResponse(&tcp.Response{Request: req, Header: map[string][]string{"Content-Type": {"application/sdp"}}, Body: body})
}

func TestH265RecoveryRejectsUnknownMode(t *testing.T) {
	producer, err := rtspHandler("rtsp://127.0.0.1:1/camera#h265_recovery=conservativ")
	require.Nil(t, producer)
	require.EqualError(t, err, "rtsp: h265_recovery must be default or conservative")
}
