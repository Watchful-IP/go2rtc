package exacq

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func socksFixture(t *testing.T, serve func(net.Conn) error) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		result <- serve(conn)
	}()
	return listener.Addr().String(), result
}

func socksGreeting(conn net.Conn, method byte) error {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 5 || header[1] == 0 {
		return errors.New("invalid SOCKS greeting")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if !bytes.Contains(methods, []byte{method}) {
		return errors.New("required SOCKS method was not offered")
	}
	return writeFull(conn, []byte{5, method})
}

func socksCredentials(conn net.Conn) (string, string, error) {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return "", "", err
	}
	if header[0] != 1 {
		return "", "", errors.New("invalid SOCKS authentication version")
	}
	user := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return "", "", err
	}
	if _, err := io.ReadFull(conn, header[:1]); err != nil {
		return "", "", err
	}
	password := make([]byte, int(header[0]))
	_, err := io.ReadFull(conn, password)
	return string(user), string(password), err
}

func socksTarget(conn net.Conn) (string, uint16, byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return "", 0, 0, err
	}
	if header[0] != 5 || header[1] != 1 || header[2] != 0 {
		return "", 0, 0, errors.New("invalid SOCKS connect request")
	}
	atyp := header[3]
	var size int
	switch atyp {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		if _, err := io.ReadFull(conn, header[:1]); err != nil {
			return "", 0, 0, err
		}
		size = int(header[0])
	default:
		return "", 0, 0, errors.New("invalid SOCKS address type")
	}
	address := make([]byte, size)
	if _, err := io.ReadFull(conn, address); err != nil {
		return "", 0, 0, err
	}
	if _, err := io.ReadFull(conn, header[:2]); err != nil {
		return "", 0, 0, err
	}
	if atyp == 3 {
		return string(address), binary.BigEndian.Uint16(header[:2]), atyp, nil
	}
	return net.IP(address).String(), binary.BigEndian.Uint16(header[:2]), atyp, nil
}

func TestSOCKSRemoteHostnameAndAuthentication(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		for _, authenticated := range []bool{false, true} {
			t.Run(scheme+map[bool]string{false: "/anonymous", true: "/authenticated"}[authenticated], func(t *testing.T) {
				const user, password = "fixture-user", "fixture-proxy-secret"
				address, result := socksFixture(t, func(conn net.Conn) error {
					method := byte(0)
					if authenticated {
						method = 2
					}
					if err := socksGreeting(conn, method); err != nil {
						return err
					}
					if authenticated {
						u, p, err := socksCredentials(conn)
						if err != nil {
							return err
						}
						if u != user || p != password {
							return errors.New("incorrect SOCKS credentials")
						}
						if err = writeFull(conn, []byte{1, 0}); err != nil {
							return err
						}
					}
					host, port, atyp, err := socksTarget(conn)
					if err != nil {
						return err
					}
					if host != "recorder.remote.invalid" || port != 22610 || atyp != 3 {
						return errors.New("recorder hostname was not passed through SOCKS")
					}
					if err = writeFull(conn, []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1}); err != nil {
						return err
					}
					var request [4]byte
					if _, err = io.ReadFull(conn, request[:]); err != nil {
						return err
					}
					if string(request[:]) != "ping" {
						return errors.New("application data bypassed proxy")
					}
					return writeFull(conn, []byte("pong"))
				})
				proxy := &url.URL{Scheme: scheme, Host: address}
				if authenticated {
					proxy.User = url.UserPassword(user, password)
				}
				conn, err := dialRecorder(context.Background(), Options{Address: "recorder.remote.invalid:22610", ProxyURL: proxy.String(), Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err = writeFull(conn, []byte("ping")); err != nil {
					t.Fatal(err)
				}
				var response [4]byte
				if _, err = io.ReadFull(conn, response[:]); err != nil || string(response[:]) != "pong" {
					t.Fatalf("proxy transport response: %v", err)
				}
				if err = <-result; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSOCKSFailureNeverDialsRecorderDirectly(t *testing.T) {
	direct, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	address, result := socksFixture(t, func(conn net.Conn) error {
		if err := socksGreeting(conn, 0); err != nil {
			return err
		}
		if _, _, _, err := socksTarget(conn); err != nil {
			return err
		}
		return writeFull(conn, []byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
	})
	conn, err := dialRecorder(context.Background(), Options{Address: direct.Addr().String(), ProxyURL: "socks5://" + address, Timeout: time.Second})
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("proxy rejection was ignored")
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	_ = direct.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if conn, err = direct.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("proxy failure caused an unauthorized direct fallback")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
}

func TestSOCKSCancellationDuringNegotiation(t *testing.T) {
	greeting := make(chan struct{})
	address, result := socksFixture(t, func(conn net.Conn) error {
		var first [2]byte
		if _, err := io.ReadFull(conn, first[:]); err != nil {
			return err
		}
		methods := make([]byte, int(first[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			return err
		}
		close(greeting)
		// Withhold the SOCKS selection reply to leave negotiation blocked.
		if _, err := conn.Read(first[:1]); err == nil {
			return errors.New("negotiation cancellation left socket writable")
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		conn, err := dialRecorder(ctx, Options{Address: "recorder.remote.invalid:22610", ProxyURL: "socks5://" + address, Timeout: time.Second})
		if conn != nil {
			_ = conn.Close()
		}
		dialed <- err
	}()
	<-greeting
	cancel()
	select {
	case err := <-dialed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SOCKS cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS negotiation ignored cancellation")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSOCKSErrorsDoNotExposeCredentials(t *testing.T) {
	const secret = "fixture-proxy-secret"
	for _, proxyURL := range []string{
		"http://user:" + secret + "@127.0.0.1:1080",
		"socks5://user:" + secret + "@127.0.0.1",
		"socks5://user:" + secret + "@127.0.0.1:0",
		"socks5://user:" + secret + "@127.0.0.1:65536",
		"socks5://user:" + secret + "@127.0.0.1:1080/path",
		"socks5://user:" + secret + "@127.0.0.1:1080?token=" + secret,
		"socks5://user:" + secret + "%zz@127.0.0.1:1080",
	} {
		conn, err := dialRecorder(context.Background(), Options{Address: "recorder.remote.invalid:22610", ProxyURL: proxyURL, Timeout: time.Second})
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("invalid proxy URL accepted or exposed credentials")
		}
	}
	address, result := socksFixture(t, func(conn net.Conn) error {
		if err := socksGreeting(conn, 2); err != nil {
			return err
		}
		if _, _, err := socksCredentials(conn); err != nil {
			return err
		}
		return writeFull(conn, []byte{1, 1})
	})
	_, err := dialRecorder(context.Background(), Options{Address: "recorder.remote.invalid:22610", ProxyURL: "socks5://user:" + secret + "@" + address, Timeout: time.Second})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("failed proxy authentication accepted or exposed credentials")
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}

func TestProxySourceOptionsAndRedaction(t *testing.T) {
	const recorderSecret, proxySecret = "fixture-recorder-secret", "fixture-proxy-secret"
	proxyURL := "socks5h://proxy-user:" + proxySecret + "@proxy.invalid:1080"
	source, err := ParseURL("exacq://user:" + recorderSecret + "@recorder.invalid/459264?proxy=" + url.QueryEscape(proxyURL))
	if err != nil {
		t.Fatal(err)
	}
	if options := source.options(); options.ProxyURL != proxyURL || options.Address != "recorder.invalid:22609" || options.Password != recorderSecret {
		t.Fatal("source proxy options lost")
	}
	redacted := source.Redacted()
	if strings.Contains(redacted, recorderSecret) || strings.Contains(redacted, proxySecret) {
		t.Fatal("source redaction retained a password")
	}
	if source.options().ProxyURL != proxyURL || source.options().Password != recorderSecret {
		t.Fatal("redaction mutated connection credentials")
	}
	// Invalid opaque URLs must either fail validation or be entirely redacted.
	malformed := "socks5:proxy-user:" + proxySecret + "@proxy.invalid:1080"
	source, err = ParseURL("exacq://user:" + recorderSecret + "@recorder.invalid/459264?proxy=" + url.QueryEscape(malformed))
	if err == nil && strings.Contains(source.Redacted(), proxySecret) {
		t.Fatal("malformed proxy redaction exposed credentials")
	}
}

func TestExplicitEmptyProxyRejected(t *testing.T) {
	if _, err := ParseURL("exacq://user:fixture-password@recorder.invalid/459264?proxy="); err == nil {
		t.Fatal("explicit empty proxy selector silently permits direct dialing")
	}
}
