package streams

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

// Watchful: make-before-break source replacement for shared camera upstreams. A
// running stream keeps serving from its current connection while the new source is
// dialled; consumers move across on the new source's first keyframe and only then is
// the old connection stopped. Consumers keep their sockets, so relays, recorders and
// players see one continuous stream.

var (
	ErrSwapNoKeyframe = errors.New("streams: no keyframe from the new source")
	ErrSwapTracks     = errors.New("streams: new source lacks a running track")
	ErrSwapRaced      = errors.New("streams: producer stopped or reconnected during swap")
)

// holdForBoundary bounds how long new video waits for the old video to finish a picture.
const holdForBoundary = 250 * time.Millisecond

// SwapSource replaces the source of a named stream without dropping its consumers,
// or creates the stream when there is none yet.
func SwapSource(name, source string, timeout time.Duration, breakFirst bool) error {
	if !HasProducer(source) {
		return errInvalidSource{errors.New("streams: source not supported")}
	}
	if err := Validate(source); err != nil {
		return errInvalidSource{err}
	}
	stream := Get(name)
	if stream == nil {
		_, err := Patch(name, source)
		return err
	}
	return stream.Swap(source, timeout, breakFirst)
}

type errInvalidSource struct{ error }

func (e errInvalidSource) Unwrap() error { return e.error }

func swapStatus(err error) int {
	var invalid errInvalidSource
	if errors.As(err, &invalid) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func swapTimeout(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 60 {
		return time.Duration(seconds) * time.Second
	}
	return 10 * time.Second
}

// Swap replaces the source of every producer in the stream. breakFirst stops the
// old connection before dialling, for sources that accept only one connection.
func (s *Stream) Swap(source string, timeout time.Duration, breakFirst bool) error {
	s.mu.Lock()
	producers := append([]*Producer(nil), s.producers...)
	s.mu.Unlock()

	for _, prod := range producers {
		swap := prod.Swap
		if breakFirst {
			swap = func(source string, _ time.Duration) error { return prod.SwapBreakFirst(source) }
		}
		if err := swap(source, timeout); err != nil {
			return err
		}
	}
	return nil
}

// Swap moves a running producer to source. A producer with nothing flowing just
// takes the new source for its next dial. On failure the old connection keeps serving.
func (p *Producer) Swap(source string, timeout time.Duration) error {
	p.mu.Lock()
	url := p.resolve(source)
	if p.state != stateStart {
		p.url = url
		p.mu.Unlock()
		return nil
	}
	old := p.conn
	receivers := append([]*core.Receiver(nil), p.receivers...)
	senders := append([]*core.Receiver(nil), p.senders...)
	p.mu.Unlock()

	conn, err := dialProducer(url)
	if err != nil {
		return err
	}

	pairs, video, err := pairTracks(conn, receivers)
	if err != nil {
		_ = conn.Stop()
		return err
	}
	for _, media := range conn.GetMedias() {
		if media.Direction != core.DirectionSendonly {
			continue
		}
		for _, sender := range senders {
			if codec := media.MatchCodec(sender.Codec); codec != nil {
				_ = conn.(core.Consumer).AddTrack(media, codec, sender)
			}
		}
	}

	var cutover *core.Cutover
	if video >= 0 {
		codec := pairs[video].To.Codec
		cutover = core.NewCutover(pairs, video,
			func(packet *core.Packet) bool { return startsKeyframe(codec, packet) },
			func(packet *core.Packet) bool { return !codec.IsRTP() || packet.Marker },
			holdForBoundary)
	} else {
		cutover = core.NewCutover(pairs, -1, nil, nil, 0)
	}

	go p.runSwapped(conn)
	cutover.Start()

	select {
	case <-cutover.Done():
	case <-time.After(timeout):
	}
	if !cutover.Moved() && cutover.Abort() {
		_ = conn.Stop()
		return ErrSwapNoKeyframe
	}

	p.mu.Lock()
	if p.conn != old || p.state != stateStart {
		p.mu.Unlock()
		_ = conn.Stop()
		return ErrSwapRaced
	}
	// The old worker must not redial the connection being retired.
	p.workerID++
	p.conn = conn
	p.url = url
	for i := range pairs {
		p.receivers[i] = pairs[i].To
	}
	cutover.MoveLateConsumers()
	p.mu.Unlock()

	_ = old.Stop()
	proto, host := sourceHost(url)
	log.Info().Str("proto", proto).Str("host", host).Msg("[streams] swapped source")
	return nil
}

// SwapBreakFirst stops the old connection before dialling source. Consumers wait on
// holders in between and resume at the new source's next keyframe. If source cannot
// be dialled the old one is dialled again, so consumers are only dropped when both fail.
func (p *Producer) SwapBreakFirst(source string) error {
	p.mu.Lock()
	url := p.resolve(source)
	if p.state != stateStart {
		p.url = url
		p.mu.Unlock()
		return nil
	}
	old, oldURL := p.conn, p.url
	receivers := append([]*core.Receiver(nil), p.receivers...)
	senders := append([]*core.Receiver(nil), p.senders...)
	holders := make([]*core.Node, len(receivers))
	for i, receiver := range receivers {
		holders[i] = &core.Node{}
		core.MoveChildren(holders[i], &receiver.Node)
	}
	// The old worker must not redial the connection being retired.
	p.workerID++
	p.mu.Unlock()
	_ = old.Stop()

	conn, err := dialProducer(url)
	var pairs []core.CutoverPair
	if err == nil {
		pairs, _, err = pairTracks(conn, receivers)
		if err != nil {
			_ = conn.Stop()
		}
	}
	swapErr := err
	if err != nil {
		url = oldURL
		if conn, err = dialProducer(url); err == nil {
			if pairs, _, err = pairTracks(conn, receivers); err != nil {
				_ = conn.Stop()
			}
		}
	}

	p.mu.Lock()
	if err != nil || p.state != stateStart {
		p.mu.Unlock()
		if err == nil {
			_ = conn.Stop()
			err = ErrSwapRaced
		}
		for _, holder := range holders {
			holder.Close()
		}
		return err
	}
	for i := range pairs {
		core.MoveChildren(&pairs[i].To.Node, holders[i])
		// Consumers that bound to the stopped track while no connection was up.
		core.MoveChildren(&pairs[i].To.Node, &receivers[i].Node)
		p.receivers[i] = pairs[i].To
	}
	for _, media := range conn.GetMedias() {
		if media.Direction != core.DirectionSendonly {
			continue
		}
		for _, sender := range senders {
			if codec := media.MatchCodec(sender.Codec); codec != nil {
				_ = conn.(core.Consumer).AddTrack(media, codec, sender)
			}
		}
	}
	p.conn = conn
	p.url = url
	workerID := p.workerID
	p.mu.Unlock()

	go p.worker(conn, workerID)
	return swapErr
}

// runSwapped is the worker for a connection started by Swap. It only reconnects once
// the connection has become the producer's current one.
func (p *Producer) runSwapped(conn core.Producer) {
	err := conn.Start()

	p.mu.Lock()
	current := p.conn == conn && p.state == stateStart
	workerID := p.workerID
	p.mu.Unlock()
	if !current {
		return
	}

	if err != nil {
		proto, host := sourceHost(p.url)
		log.Warn().Err(err).Str("proto", proto).Str("host", host).Caller().Send()
	}
	if finite, ok := conn.(core.FiniteProducer); ok && finite.IsFinite() {
		return
	}
	p.reconnect(workerID, 0)
}

func (p *Producer) resolve(source string) string {
	if p.template == "" {
		return source
	}
	return strings.Replace(p.template, SourceTemplate, source, 1)
}

// pairTracks finds a matching track on conn for every running receiver, and the
// index of the video pair to align the cutover on.
func pairTracks(conn core.Producer, receivers []*core.Receiver) ([]core.CutoverPair, int, error) {
	pairs := make([]core.CutoverPair, 0, len(receivers))
	video := -1
	for _, receiver := range receivers {
		var track *core.Receiver
		for _, media := range conn.GetMedias() {
			if media.Direction != core.DirectionRecvonly {
				continue
			}
			codec := media.MatchCodec(receiver.Codec)
			if codec == nil {
				continue
			}
			if t, err := conn.GetTrack(media, codec); err == nil {
				track = t
				break
			}
		}
		if track == nil {
			return nil, -1, ErrSwapTracks
		}
		if video < 0 && receiver.Codec.IsVideo() {
			video = len(pairs)
		}
		pairs = append(pairs, core.CutoverPair{From: receiver, To: track})
	}
	return pairs, video, nil
}

// startsKeyframe reports whether packet opens an access unit a decoder can start
// from: the parameter sets ahead of an IDR, or the IDR itself.
func startsKeyframe(codec *core.Codec, packet *core.Packet) bool {
	payload := packet.Payload
	if len(payload) == 0 {
		return false
	}
	switch codec.Name {
	case core.CodecH264:
		if !codec.IsRTP() {
			return h264.IsKeyframe(payload)
		}
		switch payload[0] & 0x1F {
		case h264.NALUTypeIFrame, h264.NALUTypeSPS:
			return true
		case 24: // STAP-A
			return len(payload) > 3 && isH264Key(payload[3]&0x1F)
		case 28: // FU-A
			return len(payload) > 1 && payload[1]&0x80 != 0 && payload[1]&0x1F == h264.NALUTypeIFrame
		}
		return false
	case core.CodecH265:
		if !codec.IsRTP() {
			return h265.IsKeyframe(payload)
		}
		switch nalType := (payload[0] >> 1) & 0x3F; nalType {
		case h265.NALUTypeAP:
			return len(payload) > 4 && isH265Key((payload[4]>>1)&0x3F)
		case h265.NALUTypeFU:
			return len(payload) > 2 && payload[2]&0x80 != 0 && isH265Key(payload[2]&0x3F)
		default:
			return isH265Key(nalType)
		}
	case core.CodecJPEG:
		// RTP JPEG: a fragment offset of zero starts a picture; every picture is a keyframe.
		return !codec.IsRTP() || len(payload) > 3 && payload[1]|payload[2]|payload[3] == 0
	}
	return true
}

func isH264Key(nalType byte) bool {
	return nalType == h264.NALUTypeIFrame || nalType == h264.NALUTypeSPS
}

func isH265Key(nalType byte) bool {
	switch nalType {
	case h265.NALUTypeVPS, h265.NALUTypeSPS, h265.NALUTypeIFrame, h265.NALUTypeIFrame2, h265.NALUTypeIFrame3:
		return true
	}
	return false
}
