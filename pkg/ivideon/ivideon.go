package ivideon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/gorilla/websocket"
)

type Producer struct {
	core.Connection
	conn *websocket.Conn

	buf []byte

	dem      *mp4.Demuxer
	done     chan struct{}
	stopOnce sync.Once
}

func Dial(source string) (core.Producer, error) {
	id := strings.Replace(source[8:], "/", ":", 1)

	url, err := GetLiveStream(id)
	if err != nil {
		return nil, err
	}

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}

	conn.SetReadLimit(64 << 20)
	prod := &Producer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "ivideon",
			Protocol:   core.Before(url, ":"), // wss
			RemoteAddr: conn.RemoteAddr().String(),
			Source:     source,
			URL:        url,
			Transport:  conn,
		},
		conn: conn,
		done: make(chan struct{}),
	}

	if err = prod.probe(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return prod, nil
}

func GetLiveStream(id string) (string, error) {
	// &video_codecs=h264,h265&audio_codecs=aac,mp3,pcma,pcmu,none
	resp, err := http.Get(
		"https://openapi-alpha.ivideon.com/cameras/" + id +
			"/live_stream?op=GET&access_token=public&q=2&video_codecs=h264&format=ws-fmp4",
	)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()
	var v struct {
		Message string `json:"message"`
		Result  struct {
			URL string `json:"url"`
		} `json:"result"`
		Success bool `json:"success"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}

	if !v.Success {
		return "", fmt.Errorf("ivideon: can't get live_stream: %s", v.Message)
	}

	return v.Result.URL, nil
}

func (p *Producer) Start() error {
	receivers := make(map[uint32]*core.Receiver)
	for _, receiver := range p.Receivers {
		trackID := p.dem.GetTrackID(receiver.Codec)
		receivers[trackID] = receiver
	}

	ch := make(chan []mp4.Sample, 10)
	done := p.done
	finished := make(chan struct{})
	defer func() { p.Stop(); <-finished }()
	go func() {
		defer close(finished)
		start := time.Now()
		var first time.Duration
		initialized := false
		for {
			select {
			case <-done:
				return
			case samples := <-ch:
				for _, sample := range samples {
					receiver := receivers[sample.TrackID]
					if receiver == nil {
						continue
					}
					packet := sample.Packet
					seconds := sample.DecodeTime / uint64(sample.TimeScale)
					if seconds > uint64(math.MaxInt64/int64(time.Second))-1 {
						p.Stop()
						return
					}
					timestamp := time.Duration(seconds)*time.Second + time.Duration(sample.DecodeTime%uint64(sample.TimeScale))*time.Second/time.Duration(sample.TimeScale)
					if !initialized {
						first = timestamp
						initialized = true
					}
					elapsed := timestamp - first
					timer := time.NewTimer(max(0, elapsed-time.Since(start)))
					select {
					case <-done:
						timer.Stop()
						return
					case <-timer.C:
					}
					receiver.WriteRTP(packet)
				}
			}
		}
	}()
	samples, err := p.dem.Demux(p.buf)
	if err != nil {
		return err
	}
	select {
	case ch <- samples:
	case <-done:
		return nil
	}

	for {
		var msg message
		if err := p.conn.ReadJSON(&msg); err != nil {
			return err
		}

		switch msg.Type {
		case "stream-init", "metadata":
			continue

		case "fragment":
			_, b, err := p.conn.ReadMessage()
			if err != nil {
				return err
			}

			p.Recv += len(b)
			samples, err := p.dem.Demux(b)
			if err != nil {
				return err
			}
			select {
			case ch <- samples:
			case <-done:
				return nil
			}

		default:
			return errors.New("ivideon: wrong message type: " + msg.Type)
		}
	}
}

func (p *Producer) probe() (err error) {
	p.dem = &mp4.Demuxer{}

	for {
		var msg message
		if err = p.conn.ReadJSON(&msg); err != nil {
			return err
		}

		switch msg.Type {
		case "metadata":
			continue

		case "stream-init":
			// it's difficult to maintain audio
			if strings.HasPrefix(msg.CodecString, "avc1") {
				medias, err := p.dem.Probe(msg.Data)
				if err != nil {
					return err
				}
				p.Medias = append(p.Medias, medias...)
			}

		case "fragment":
			_, p.buf, err = p.conn.ReadMessage()
			return

		default:
			return errors.New("ivideon: wrong message type: " + msg.Type)
		}
	}
}

type message struct {
	Type        string `json:"type"`
	CodecString string `json:"codec_string"`
	Data        []byte `json:"data"`
	//TrackID     byte    `json:"track_id"`
	//Track       byte    `json:"track"`
	//StartTime   float32 `json:"start_time"`
	//Duration    float32 `json:"duration"`
	//IsKey       bool    `json:"is_key"`
	//DataOffset  uint32  `json:"data_offset"`
}

func (p *Producer) Stop() error {
	p.stopOnce.Do(func() { close(p.done); _ = p.Connection.Stop() })
	return nil
}
