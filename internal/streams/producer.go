package streams

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

type state byte

const (
	stateNone state = iota
	stateMedias
	stateTracks
	stateStart
	stateExternal
	stateInternal
)

type Producer struct {
	core.Listener

	url      string
	template string

	conn      core.Producer
	receivers []*core.Receiver
	senders   []*core.Receiver

	state    state
	mu       sync.Mutex
	workerID int
}

const SourceTemplate = "{input}"

func NewProducer(source string) *Producer {
	if strings.Contains(source, SourceTemplate) {
		return &Producer{template: source}
	}

	return &Producer{url: source}
}

func (p *Producer) SetSource(s string) {
	if p.template == "" {
		p.url = s
	} else {
		p.url = strings.Replace(p.template, SourceTemplate, s, 1)
	}
}

func (p *Producer) Dial() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		conn, err := dialProducer(p.url)
		if err != nil {
			return err
		}

		p.conn = conn
		p.state = stateMedias
	}

	return nil
}

func (p *Producer) GetMedias() []*core.Media {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		return nil
	}

	return p.conn.GetMedias()
}

func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		return nil, errors.New("get track from none state")
	}

	for _, track := range p.receivers {
		if track.Codec == codec {
			return track, nil
		}
	}

	track, err := p.conn.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}

	p.receivers = append(p.receivers, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return track, nil
}

func (p *Producer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		return errors.New("add track from none state")
	}

	if err := p.conn.(core.Consumer).AddTrack(media, codec, track); err != nil {
		return err
	}

	p.senders = append(p.senders, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return nil
}

func (p *Producer) MarshalJSON() ([]byte, error) {
	if conn := p.conn; conn != nil {
		return json.Marshal(conn)
	}
	info := map[string]string{"url": p.url}
	return json.Marshal(info)
}

// internals

func (p *Producer) start() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != stateTracks {
		return
	}

	log.Debug().Msgf("[streams] start producer url=%s", p.url)

	p.state = stateStart
	p.workerID++

	go p.worker(p.conn, p.workerID)
}

func (p *Producer) worker(conn core.Producer, workerID int) {
	if err := conn.Start(); err != nil {
		p.mu.Lock()
		closed := p.workerID != workerID
		p.mu.Unlock()

		if closed {
			return
		}

		proto, host := sourceHost(p.url)
		log.Warn().Err(err).Str("proto", proto).Str("host", host).Caller().Send()
	}

	if finite, ok := conn.(core.FiniteProducer); ok && finite.IsFinite() {
		return
	}
	p.reconnect(workerID, 0)
}

func (p *Producer) reconnect(workerID, retry int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.workerID != workerID {
		log.Trace().Msgf("[streams] stop reconnect url=%s", p.url)
		return
	}

	log.Debug().Msgf("[streams] retry=%d to url=%s", retry, p.url)

	conn, err := dialProducer(p.url)
	if err != nil {
		log.Debug().Msgf("[streams] producer=%s", err)

		timeout := time.Minute
		if retry < 5 {
			timeout = time.Second
		} else if retry < 10 {
			timeout = time.Second * 5
		} else if retry < 20 {
			timeout = time.Second * 10
		}

		time.AfterFunc(timeout, func() {
			p.reconnect(workerID, retry+1)
		})
		return
	}

	for _, media := range conn.GetMedias() {
		switch media.Direction {
		case core.DirectionRecvonly:
			for i, receiver := range p.receivers {
				codec := media.MatchCodec(receiver.Codec)
				if codec == nil {
					continue
				}

				track, err := conn.GetTrack(media, codec)
				if err != nil {
					continue
				}

				receiver.Replace(track)
				p.receivers[i] = track
				break
			}

		case core.DirectionSendonly:
			for _, sender := range p.senders {
				codec := media.MatchCodec(sender.Codec)
				if codec == nil {
					continue
				}

				_ = conn.(core.Consumer).AddTrack(media, codec, sender)
			}
		}
	}

	// stop previous connection after moving tracks (fix ghost exec/ffmpeg)
	_ = p.conn.Stop()
	// swap connections
	p.conn = conn

	go p.worker(conn, workerID)
}

// Watchful: log every upstream connection attempt, including reconnects. Only
// protocol and host are logged; source URLs carry credentials and tokens.
func dialProducer(url string) (core.Producer, error) {
	conn, err := GetProducer(url)
	proto, host := sourceHost(url)
	log.Info().Str("proto", proto).Str("host", host).Bool("ok", err == nil).Msg("[streams] dial")
	return conn, err
}

// sourceHost returns the scheme and host[:port] of the innermost URL in a source,
// e.g. ffmpeg:rtsp://user:pass@cam:554/x#video=copy -> ("rtsp", "cam:554").
func sourceHost(source string) (proto, host string) {
	i := strings.LastIndex(source, "://")
	if i < 0 {
		proto, _, _ = strings.Cut(source, ":")
		return proto, ""
	}
	proto = source[strings.LastIndexAny(source[:i], ": ")+1 : i]
	host = source[i+3:]
	if j := strings.IndexAny(host, " ?#"); j >= 0 {
		host = host[:j]
	}
	// strip userinfo first, so a '/' inside a password can't end the host early
	if j := strings.LastIndexByte(host, '@'); j >= 0 {
		host = host[j+1:]
	}
	if j := strings.IndexByte(host, '/'); j >= 0 {
		host = host[:j]
	}
	return proto, host
}

func (p *Producer) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch p.state {
	case stateExternal:
		log.Trace().Msgf("[streams] skip stop external producer")
		return
	case stateNone:
		log.Trace().Msgf("[streams] skip stop none producer")
		return
	case stateStart:
		p.workerID++
	}

	log.Debug().Msgf("[streams] stop producer url=%s", p.url)

	if p.conn != nil {
		_ = p.conn.Stop()
		p.conn = nil
	}

	p.state = stateNone
	p.receivers = nil
	p.senders = nil
}
