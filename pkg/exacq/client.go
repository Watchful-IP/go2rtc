package exacq

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// Options describes one recorder connection. All operations use Address; media
// and control never follow recorder-advertised addresses onto a second port.
type Options struct {
	Address  string
	Username string
	Password string
	ProxyURL string
	Timeout  time.Duration
	// Trace, when set, receives phase timings and recorder identity.
	Trace *Trace
}

type Discovery struct {
	Serial              string `xml:"Serial,attr"`
	NegotiateMechanisms uint32 `xml:"NegotiateMechanisms,attr"`
}

// Client owns one native session. Reads are serialized by the operation using
// it; callers use separate sessions for independent streaming and control.
type Client struct {
	conn            net.Conn
	options         Options
	discovery       Discovery
	txKey, rxKey    []byte
	loginKeyPending bool
	writeMu         sync.Mutex
	closeOnce       sync.Once
	closeErr        error
	done            chan struct{}
	inventoryMu     sync.RWMutex
	inventory       *Inventory
	readMu          sync.Mutex
	pendingKey      []byte
}

func openTransport(ctx context.Context, options Options) (*Client, error) {
	if options.Address == "" {
		return nil, errorf(CodeInvalidRequest, PhaseDial, "recorder address is required")
	}
	if options.Timeout <= 0 {
		options.Timeout = 15 * time.Second
	}
	options.Trace.connect()
	start := time.Now()
	conn, err := dialRecorder(ctx, options)
	options.Trace.Observe(PhaseDial, start)
	if err != nil {
		return nil, dialError(err, options.Address, options.ProxyURL != "")
	}
	c := &Client{conn: conn, options: options, txKey: make([]byte, 16), rxKey: make([]byte, 16), done: make(chan struct{})}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	start = time.Now()
	err = c.handshake()
	options.Trace.Observe(PhaseHandshake, start)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = c.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, c.handshakeError(err)
	}
	options.Trace.identify(c.discovery.Serial, "")
	return c, nil
}

func dialRecorder(ctx context.Context, options Options) (net.Conn, error) {
	direct := &net.Dialer{Timeout: options.Timeout}
	if options.ProxyURL == "" {
		return direct.DialContext(ctx, "tcp", options.Address)
	}
	u, err := parseProxy(options.ProxyURL)
	if err != nil {
		return nil, err
	}
	var auth *proxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, direct)
	if err != nil {
		return nil, errors.New("exacq: unable to configure SOCKS proxy")
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("exacq: SOCKS proxy does not support cancellation")
	}
	bounded, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	// SOCKS receives the recorder hostname directly, preserving remote DNS and
	// site routing. A proxy failure never falls back to a direct connection.
	conn, err := contextDialer.DialContext(bounded, "tcp", options.Address)
	if bounded.Err() != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, bounded.Err()
	}
	return conn, err
}

func parseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("exacq: invalid SOCKS proxy configuration")
	}
	if n, e := strconv.ParseUint(u.Port(), 10, 16); e != nil || n == 0 {
		return nil, errors.New("exacq: SOCKS proxy requires a valid port")
	}
	return u, nil
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// greetingTimeout bounds the wait for the recorder's first packet, which it
// sends immediately on accept. Services that wait for the client (HTTP on the
// Web Service port) fail fast instead of consuming the whole timeout.
const greetingTimeout = 5 * time.Second

// handshake establishes the transport and reads the server's advertised auth
// mechanisms. It does not authenticate a user.
func (c *Client) handshake() error {
	deadline := time.Now().Add(c.options.Timeout)
	_ = c.conn.SetReadDeadline(time.Now().Add(min(c.options.Timeout, greetingTimeout)))
	var pending []byte
	for count := 0; count < 16; count++ {
		var p *packet
		var err error
		if count == 0 {
			p, err = c.readGreeting()
			_ = c.conn.SetReadDeadline(deadline)
		} else {
			p, err = c.readPacket()
		}
		if err != nil {
			return err
		}
		switch {
		case p.Type == packetKey && p.Subtype == 1:
			var response []byte
			response, pending, err = legacyDHResponse(p.Payload, rand.Reader)
			if err != nil {
				return err
			}
			if err = c.sendPlain(packetKey, 2, response); err != nil {
				return err
			}
		case p.Type == packetKey && p.Subtype == 3:
			if len(pending) == 0 {
				return errors.New("exacq: unexpected key acknowledgement")
			}
			c.txKey, c.rxKey = pending, pending
			pending = nil
			body := []byte(`<?xml version="1.0" encoding="UTF-8"?><eDVR Delta="1"><Client HeartbeatTimeout="30" MaxPacket="4194303" ConnectionSupports="159" ConnectionName="go2rtc" ConnectionVersion="(1.47.27.1)"/></eDVR>`)
			if err = c.sendPacket(packetDiscover, 1, body); err != nil {
				return err
			}
		case p.Type == packetDiscover && p.Subtype == 2:
			var doc struct {
				Servers []Discovery `xml:"Servers>Server"`
			}
			if err = decodeXML(p.Payload, &doc); err != nil {
				return fmt.Errorf("exacq: discovery: %w", err)
			}
			if len(doc.Servers) != 1 {
				return fmt.Errorf("exacq: expected one recorder in discovery, got %d", len(doc.Servers))
			}
			c.discovery = doc.Servers[0]
			_ = c.conn.SetReadDeadline(time.Time{})
			return nil
		default:
			return fmt.Errorf("exacq: unexpected handshake packet %d/%d", p.Type, p.Subtype)
		}
	}
	return errors.New("exacq: handshake message limit exceeded")
}

// greetingReader records the first bytes a peer sends so a failed greeting
// can say what actually answered on the port.
type greetingReader struct {
	r    io.Reader
	n    int
	head []byte
}

func (g *greetingReader) Read(b []byte) (int, error) {
	n, err := g.r.Read(b)
	g.n += n
	if len(g.head) < 8 {
		g.head = append(g.head, b[:min(n, 8-len(g.head))]...)
	}
	return n, err
}

// errNotExacq marks a greeting that cannot be an Exacq packet.
type errNotExacq struct{ head []byte }

func (e errNotExacq) Error() string { return "exacq: peer is not an exacqVision Server" }

func (c *Client) readGreeting() (*packet, error) {
	g := &greetingReader{r: c.conn}
	p, err := readPacket(g)
	if err != nil {
		if g.n > 0 && !connectionLost(err) {
			return nil, errNotExacq{g.head}
		}
		if g.n > 0 && (bytes.HasPrefix(g.head, []byte("HTTP/")) || g.head[0] == 0x15 || g.head[0] == 0x16) {
			return nil, errNotExacq{g.head}
		}
		return nil, err
	}
	return c.decodePacket(p)
}

// handshakeError explains why the peer could not be identified as a recorder.
func (c *Client) handshakeError(err error) error {
	address := c.options.Address
	var notExacq errNotExacq
	var netErr net.Error
	switch {
	case AsError(err) != nil:
		return err
	case errors.As(err, &notExacq):
		what := "an unrecognised protocol"
		switch {
		case bytes.HasPrefix(notExacq.head, []byte("HTTP/")):
			what = "HTTP"
		case len(notExacq.head) > 0 && (notExacq.head[0] == 0x15 || notExacq.head[0] == 0x16):
			what = "TLS"
		case bytes.HasPrefix(notExacq.head, []byte("SSH-")):
			what = "SSH"
		}
		return wrapf(err, CodeNotExacq, PhaseHandshake, "%s answered with %s, not the exacqVision Server native protocol (default port 22609); check NativePort is not the Web Service port", address, what)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return wrapf(err, CodeNotExacq, PhaseHandshake, "%s accepted the connection but sent no exacqVision greeting within %s; it is probably not the exacqVision Server native port (default 22609)", address, min(c.options.Timeout, greetingTimeout))
	case connectionLost(err):
		return wrapf(err, CodeConnectionLost, PhaseHandshake, "%s closed the connection before identifying as an exacqVision Server; check NativePort (default 22609) and that the server accepts remote clients", address)
	}
	return classify(err, PhaseHandshake)
}

func (c *Client) readPacket() (*packet, error) {
	p, err := readPacket(c.conn)
	if err != nil {
		return nil, err
	}
	return c.decodePacket(p)
}

func (c *Client) decodePacket(p *packet) (*packet, error) {
	var err error
	// The successful SPAKE acknowledgement is the first server packet using
	// the replacement key. Its unencrypted header identifies the transition.
	if c.loginKeyPending && p.Type == packetLogin && p.Subtype == 2 && p.Status == 0 {
		if p.Encryption != 1 {
			return nil, errors.New("exacq: unencrypted authentication acknowledgement")
		}
		c.rxKey = c.txKey
		c.loginKeyPending = false
	}
	switch p.Encryption {
	case 0:
	case 1:
		p.Payload, err = decryptPayload(p.Payload, c.rxKey)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("exacq: unsupported encryption %d", p.Encryption)
	}
	p.Payload, err = decompressPayload(p.Payload, p.Compression)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Client) sendPacket(kind, subtype uint32, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	payload, err := compressPayload(body)
	if err != nil {
		return err
	}
	payload, err = encryptPayload(payload, c.txKey, rand.Reader)
	if err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.options.Timeout))
	return writePacket(c.conn, &packet{Type: kind, Subtype: subtype, Compression: 1, Encryption: 1, Payload: payload})
}

func (c *Client) sendPlain(kind, subtype uint32, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.options.Timeout))
	return writePacket(c.conn, &packet{Type: kind, Subtype: subtype, Payload: body})
}

func decodeXML(data []byte, value any) error {
	// The recorder emits the non-conforming declaration "<?xml ?>". Strip
	// only this known vendor prefix, leaving strict XML parsing for the body.
	data = bytes.TrimPrefix(data, []byte("<?xml ?>"))
	data = bytes.TrimRight(data, "\x00")
	return xml.Unmarshal(data, value)
}
