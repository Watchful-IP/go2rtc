package exacq

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	e := AsError(err)
	require.NotNil(t, e, "unclassified error: %v", err)
	require.Equal(t, code, e.Code, e.Message)
	require.Equal(t, retryable(code), e.Retryable)
	return e
}

func TestLoginStatusCodes(t *testing.T) {
	for status, code := range map[int]Code{
		-3: CodeAuthInvalidCredentials, -2: CodeAuthNoAccounts, -4: CodeAuthScheduleBlocked,
		1: CodeAuthDirectoryFailed, 2: CodeAuthDirectoryFailed, 3: CodeAuthDirectoryFailed,
		4: CodeAuthUnsupported, 7: CodeAuthUnsupported, -1: CodeAuthRejected, 5: CodeAuthRejected,
	} {
		e := requireCode(t, loginError(uint16(int16(status))), code)
		require.Equal(t, status, *e.RecorderStatus)
		require.Equal(t, PhaseLogin, e.Phase)
	}
}

func TestSearchAndConfigStatusCodes(t *testing.T) {
	requireCode(t, searchRejected(1), CodePermissionDenied)
	requireCode(t, searchRejected(2), CodeStreamUnavailable)
	requireCode(t, searchRejected(4), CodeStreamUnavailable)
	e := requireCode(t, configRejected(PhaseWebhook, 0xffff), CodeConfigRejected)
	require.Equal(t, -1, *e.RecorderStatus)
	require.Contains(t, e.Message, "Setup or administrator permission")
}

func TestClassifyTransportConditions(t *testing.T) {
	requireCode(t, classify(io.EOF, PhaseStatus), CodeConnectionLost)
	requireCode(t, classify(context.DeadlineExceeded, PhaseSearch), CodeTimeout)
	e := requireCode(t, classify(errors.New("exacq: status response limit exceeded"), PhaseStatus), CodeProtocolError)
	require.Equal(t, "unexpected recorder response during status: status response limit exceeded", e.Message)
	typed := errorf(CodeCameraNotFound, "", "camera 1 is not on this recorder")
	requireCode(t, classify(typed, PhaseLive), CodeCameraNotFound)
	require.Equal(t, PhaseLive, typed.Phase)
	require.Nil(t, classify(nil, PhaseLive))
	require.True(t, errors.Is(classify(ErrNoSnapshot, PhaseSnapshot), ErrNoSnapshot))
}

func closedPort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

// peer accepts one connection and runs serve on it.
func peer(t *testing.T, serve func(net.Conn)) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}()
	return listener.Addr().String()
}

func TestTransportClassification(t *testing.T) {
	ctx := context.Background()
	address := closedPort(t)
	_, err := openTransport(ctx, Options{Address: address, Timeout: time.Second})
	e := requireCode(t, err, CodeUnreachable)
	require.Contains(t, e.Message, "connection refused by "+address)
	require.Equal(t, PhaseDial, e.Phase)

	address = peer(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
	})
	_, err = openTransport(ctx, Options{Address: address, Timeout: time.Second})
	e = requireCode(t, err, CodeNotExacq)
	require.Contains(t, e.Message, "answered with HTTP")
	require.Equal(t, PhaseHandshake, e.Phase)

	address = peer(t, func(conn net.Conn) { time.Sleep(time.Second) })
	_, err = openTransport(ctx, Options{Address: address, Timeout: 100 * time.Millisecond})
	e = requireCode(t, err, CodeNotExacq)
	require.Contains(t, e.Message, "sent no exacqVision greeting")

	address = peer(t, func(net.Conn) {})
	_, err = openTransport(ctx, Options{Address: address, Timeout: time.Second})
	requireCode(t, err, CodeConnectionLost)

	_, err = openTransport(ctx, Options{Address: "recorder.invalid:22609", Timeout: time.Second})
	e = requireCode(t, err, CodeUnreachable)
	require.Contains(t, e.Message, `could not resolve recorder host "recorder.invalid"`)
}

func TestSOCKSClassification(t *testing.T) {
	ctx := context.Background()
	proxy := closedPort(t)
	_, err := openTransport(ctx, Options{Address: "recorder:22609", ProxyURL: "socks5://route-secret:bridge-secret@" + proxy, Timeout: time.Second})
	e := requireCode(t, err, CodeProxyFailed)
	require.Contains(t, e.Message, "could not reach the EdgeLink proxy")
	require.NotContains(t, e.Message, "secret")

	for reply, reason := range map[byte]string{4: "host unreachable", 5: "connection refused", 3: "network unreachable"} {
		proxy, result := socksFixture(t, func(conn net.Conn) error {
			if err := socksGreeting(conn, 2); err != nil {
				return err
			}
			if _, _, err := socksCredentials(conn); err != nil {
				return err
			}
			if err := writeFull(conn, []byte{1, 0}); err != nil {
				return err
			}
			if _, _, _, err := socksTarget(conn); err != nil {
				return err
			}
			return writeFull(conn, []byte{5, reply, 0, 1, 0, 0, 0, 0, 0, 0})
		})
		_, err = openTransport(ctx, Options{Address: "recorder:22609", ProxyURL: "socks5://route-secret:bridge-secret@" + proxy, Timeout: time.Second})
		require.NoError(t, <-result)
		e = requireCode(t, err, CodeUnreachable)
		require.Equal(t, "EdgeLink could not reach the recorder at recorder:22609 ("+reason+")", e.Message)
	}

	proxy, result := socksFixture(t, func(conn net.Conn) error {
		if err := socksGreeting(conn, 2); err != nil {
			return err
		}
		if _, _, err := socksCredentials(conn); err != nil {
			return err
		}
		return writeFull(conn, []byte{1, 1})
	})
	_, err = openTransport(ctx, Options{Address: "recorder:22609", ProxyURL: "socks5://route-secret:bridge-secret@" + proxy, Timeout: time.Second})
	require.NoError(t, <-result)
	e = requireCode(t, err, CodeProxyFailed)
	require.True(t, strings.HasPrefix(e.Message, "EdgeLink proxy refused the route to recorder:22609"), e.Message)
	require.NotContains(t, e.Message, "secret")
}

func TestWebhookUnsupportedNamesVersion(t *testing.T) {
	inv := &Inventory{Server: ServerInfo{Version: "9.4.1"}, Plugins: map[string]uint32{"EventPI": 655360}}
	_, _, err := planWebhook(inv, webhookTestConfig())
	e := requireCode(t, err, CodeWebhookUnsupported)
	require.Contains(t, e.Message, "exacqVision 9.4.1")
}
