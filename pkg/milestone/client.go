package milestone

import (
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

const defaultPort = "7563"

// keepaliveTimeout: the recorder sends a livepackage at least every 5 seconds
// while live, and answers playback requests in well under that.
const keepaliveTimeout = 15 * time.Second

var reGUID = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// Source is a parsed milestone:// URL.
//
//	milestone[s|x]://recorder[:7563]/<camera-guid>?token=<connection-token>
//	  [&stream=<stream-guid>][&start=<time>[&end=<time>][&speed=<x>]][&jpeg=1]
//
// milestones verifies the recorder certificate, milestonex does not.
// Times are recorder clock: unix milliseconds or RFC 3339.
type Source struct {
	URL    *url.URL
	Camera string
	Stream string
	Token  string
	JPEG   bool

	Playback bool
	Start    time.Time
	End      time.Time // zero = until the recording ends
	Speed    float64
}

func ParseURL(rawURL string) (*Source, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "milestone", "milestones", "milestonex":
	default:
		return nil, errors.New("milestone: unsupported scheme " + u.Scheme)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), defaultPort)
	}

	q := u.Query()
	s := &Source{
		URL:    u,
		Camera: strings.Trim(u.Path, "/"),
		Stream: q.Get("stream"),
		Token:  q.Get("token"),
		JPEG:   q.Get("jpeg") == "1",
		Speed:  1,
	}
	if !reGUID.MatchString(s.Camera) {
		return nil, errors.New("milestone: path must be the camera GUID")
	}
	if s.Stream != "" && !reGUID.MatchString(s.Stream) {
		return nil, errors.New("milestone: stream must be a GUID")
	}
	if s.Token == "" {
		return nil, errors.New("milestone: token is required")
	}
	if v := q.Get("start"); v != "" {
		if s.Start, err = parseTime(v); err != nil {
			return nil, err
		}
		s.Playback = true
	}
	if v := q.Get("end"); v != "" {
		if !s.Playback {
			return nil, errors.New("milestone: end requires start")
		}
		if s.End, err = parseTime(v); err != nil {
			return nil, err
		}
		if !s.End.After(s.Start) {
			return nil, errors.New("milestone: end must be after start")
		}
	}
	if v := q.Get("speed"); v != "" {
		if s.Speed, err = strconv.ParseFloat(v, 64); err != nil || s.Speed <= 0 || s.Speed > 32 {
			return nil, errors.New("milestone: speed must be in (0, 32]")
		}
	}
	return s, nil
}

func parseTime(v string) (time.Time, error) {
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("milestone: invalid time " + v)
}

// Redacted is the URL without the connection token, for logs and the API.
func (s *Source) Redacted() string {
	u := *s.URL
	q := u.Query()
	if q.Has("token") {
		q.Set("token", "xxx")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Client is one ImageServer connection for one camera.
type Client struct {
	src   *Source
	conn  net.Conn
	rd    *reader
	reqID int
	jpeg  bool
}

func dialClient(src *Source, jpeg bool) (*Client, error) {
	conn, err := net.DialTimeout("tcp", src.URL.Host, core.ConnDialTimeout)
	if err != nil {
		return nil, err
	}
	if src.URL.Scheme != "milestone" {
		host := src.URL.Hostname()
		conf := &tls.Config{ServerName: host}
		if src.URL.Scheme == "milestonex" || net.ParseIP(host) != nil {
			// Recorder certificates name the Windows host, which rarely resolves.
			conf = &tls.Config{InsecureSkipVerify: true}
		}
		tlsConn := tls.Client(conn, conf)
		_ = tlsConn.SetDeadline(time.Now().Add(core.ConnDialTimeout))
		if err = tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = tlsConn.SetDeadline(time.Time{})
		conn = tlsConn
	}

	c := &Client{src: src, conn: conn, rd: newReader(conn), jpeg: jpeg}
	if err = c.connect(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) connect() error {
	param := "id=" + c.src.Camera
	if c.src.Stream != "" {
		param += "&amp;streamid=" + c.src.Stream
	}
	param += "&amp;connectiontoken=" + xmlEscape(c.src.Token)

	stdJPEG := "no"
	if c.jpeg {
		stdJPEG = "yes"
	}
	// Username and password are required elements; the token authenticates.
	params := `<username>a</username><password>a</password><cameraid>` + c.src.Camera +
		`</cameraid><alwaysstdjpeg>` + stdJPEG + `</alwaysstdjpeg><connectparam>` + param +
		`</connectparam><clientcapabilities><privacymask>no</privacymask><multipartdata>yes</multipartdata>` +
		`<datarestriction>no</datarestriction></clientcapabilities><transcode><allframes>yes</allframes></transcode>`
	if err := c.send("connect", params); err != nil {
		return err
	}
	for {
		res, err := c.read()
		if err != nil {
			return err
		}
		if !res.isXML() {
			continue
		}
		if xmlValue(res.xml, "methodname") != "connect" {
			continue
		}
		if xmlValue(res.xml, "connected") != "yes" {
			return methodError(res.xml)
		}
		return nil
	}
}

func (c *Client) send(method, params string) error {
	c.reqID++
	_ = c.conn.SetWriteDeadline(time.Now().Add(core.ConnDialTimeout))
	_, err := c.conn.Write(methodCall(c.reqID, method, params))
	return err
}

func (c *Client) read() (*response, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(keepaliveTimeout))
	return c.rd.read()
}

func (c *Client) Live() error {
	// Without sendinitialimage the recorder starts with a periodic JPEG snapshot.
	return c.send("live", "<sendinitialimage>no</sendinitialimage><adaptivestreaming><disabled/></adaptivestreaming>")
}

func (c *Client) Goto(t time.Time) error {
	return c.send("goto", "<time>"+strconv.FormatInt(t.UnixMilli(), 10)+"</time>")
}

func (c *Client) Next() error {
	return c.send("next", "")
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
