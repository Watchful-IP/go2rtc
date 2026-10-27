package exacq

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	native "github.com/Watchful-IP/ihub/servers/exacq/exacq"
)

// Source identifies a camera and an optional finite recording interval.
//
// exacq://user:password@recorder[:22609]/camera[?stream=ID|select=lowest]
// exacq://user:password@recorder[:22609]/camera?start=epochms&end=epochms&speed=1
//
// Recording gaps are preserved. Camera and stream IDs are the recorder's
// inventory IDs. select=lowest picks the camera's smallest enabled H.264/H.265
// stream from the session inventory.
type Source struct {
	URL          *url.URL
	Camera       uint32
	Stream       uint32
	SelectLowest bool
	Playback     bool
	Start, End   time.Time
	Speed        float64
}

// ParseURL classifies every rejection as invalid_request.
func ParseURL(rawURL string) (*Source, error) {
	s, err := parseURL(rawURL)
	if err != nil {
		return nil, wrapf(err, native.CodeInvalidRequest, native.PhaseRequest, "%s", strings.TrimPrefix(err.Error(), "exacq: "))
	}
	return s, nil
}

func parseURL(rawURL string) (*Source, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("exacq: invalid source URL")
	}
	if u.Scheme != "exacq" {
		return nil, errors.New("exacq: unsupported source scheme")
	}
	if u.Hostname() == "" {
		return nil, errors.New("exacq: recorder host is required")
	}
	if u.Fragment != "" {
		return nil, errors.New("exacq: source fragments are unsupported")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("exacq: recorder username is required")
	}
	if password, present := u.User.Password(); !present || password == "" {
		return nil, errors.New("exacq: recorder password is required")
	}
	if port := u.Port(); port == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "22609")
	} else if n, e := strconv.ParseUint(port, 10, 16); e != nil || n == 0 {
		return nil, errors.New("exacq: invalid recorder port")
	}
	camera, err := parseID(strings.TrimPrefix(u.Path, "/"))
	if err != nil {
		return nil, errors.New("exacq: path must contain one nonzero camera ID")
	}
	s := &Source{URL: u, Camera: camera, Speed: 1}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("exacq: invalid query")
	}
	for key, values := range q {
		if len(values) != 1 {
			return nil, errors.New("exacq: duplicate source parameter")
		}
		switch key {
		case "stream", "select", "start", "end", "speed", "gaps":
		default:
			return nil, errors.New("exacq: unsupported source parameter")
		}
	}
	if q.Has("stream") {
		if s.Stream, err = parseID(q.Get("stream")); err != nil {
			return nil, errors.New("exacq: invalid stream ID")
		}
	}
	if q.Has("select") {
		if q.Get("select") != "lowest" {
			return nil, errors.New("exacq: select must be lowest")
		}
		if q.Has("stream") {
			return nil, errors.New("exacq: stream and select=lowest are mutually exclusive")
		}
		s.SelectLowest = true
	}
	if q.Has("start") || q.Has("end") {
		if !q.Has("start") || !q.Has("end") {
			return nil, errors.New("exacq: playback requires start and end")
		}
		if s.Start, err = parseSourceTime(q.Get("start")); err != nil {
			return nil, err
		}
		if s.End, err = parseSourceTime(q.Get("end")); err != nil {
			return nil, err
		}
		if !s.End.After(s.Start) || s.End.Sub(s.Start) > time.Hour {
			return nil, errors.New("exacq: playback range must be positive and at most one hour")
		}
		s.Playback = true
	}
	if q.Has("speed") {
		if !s.Playback {
			return nil, errors.New("exacq: speed requires playback")
		}
		if s.Speed, err = strconv.ParseFloat(q.Get("speed"), 64); err != nil || math.IsNaN(s.Speed) || math.IsInf(s.Speed, 0) || s.Speed <= 0 || s.Speed > 32 {
			return nil, errors.New("exacq: speed must be in (0,32]")
		}
	}
	if q.Has("gaps") && q.Get("gaps") != "preserve" {
		return nil, errors.New("exacq: gaps must be preserve")
	}
	return s, nil
}

func parseID(value string) (uint32, error) {
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n == 0 {
		return 0, errors.New("exacq: invalid ID")
	}
	return uint32(n), nil
}

func parseSourceTime(value string) (time.Time, error) {
	var t time.Time
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		t = time.UnixMilli(n)
	} else {
		t, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, errors.New("exacq: invalid recording time")
		}
	}
	// Recorded search times are signed 32-bit seconds.
	if t.Unix() < 0 || t.Unix() > math.MaxInt32 {
		return time.Time{}, errors.New("exacq: recording time is outside the supported range")
	}
	return t, nil
}

// Redacted is the source URL without recorder credentials.
func (s *Source) Redacted() string {
	u := *s.URL
	if u.User != nil {
		u.User = url.UserPassword("xxx", "xxx")
	}
	return u.String()
}

// options requests live video on stream (when nonzero) with the login, so its
// media follows the configuration without another round trip.
func (s *Source) options(stream uint32) native.Options {
	password, _ := s.URL.User.Password()
	return native.Options{Address: s.URL.Host, Username: s.URL.User.Username(), Password: password, PrestartLive: stream}
}

// liveStream is the live stream known before the inventory is read: the named
// stream or the camera's own. select=lowest needs the inventory to choose.
func (s *Source) liveStream() uint32 {
	switch {
	case s.Playback || s.SelectLowest:
		return 0
	case s.Stream != 0:
		return s.Stream
	}
	return s.Camera
}

func failf(code native.Code, phase, format string, args ...any) error {
	return &native.Error{Code: code, Phase: phase, Message: fmt.Sprintf(format, args...)}
}

func wrapf(err error, code native.Code, phase, format string, args ...any) error {
	return &native.Error{Code: code, Phase: phase, Message: fmt.Sprintf(format, args...), Err: err}
}
