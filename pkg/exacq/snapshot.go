package exacq

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os/exec"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/reorder"
)

var ErrSnapshotDecoderUnavailable error = errorf(CodeDecoderUnavailable, PhaseSnapshot, "snapshots require FFmpeg in PATH on the go2rtc host")
var ErrNoSnapshot error = errorf(CodeNoRecording, PhaseSnapshot, "no decodable picture in the requested interval; the camera may not have been recording then")

type Snapshot struct {
	JPEG       []byte    `json:"-"`
	CapturedAt time.Time `json:"capturedAt"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
}

// Snapshot consumes and closes this dedicated client, including on failure.
// A nil at requests a fresh timed live keyframe. A historical request selects
// the first recorded picture at or after at, within three seconds, retaining
// the preceding GOP for decoding. It never falls back to live video. Width 0
// keeps the original width up to 4096; a positive width is an upper bound.
// Video remains native Go; only this optional JPEG conversion invokes FFmpeg.
func (c *Client) Snapshot(ctx context.Context, cameraID uint32, width int, at *time.Time) (*Snapshot, error) {
	defer c.Close()
	if width < 0 || width > 4096 {
		return nil, errorf(CodeInvalidRequest, PhaseSnapshot, "snapshot width must be between 0 and 4096")
	}
	if at != nil && (at.Unix() <= 0 || at.Add(3*time.Second).After(time.Unix(math.MaxInt32, 0))) {
		return nil, errorf(CodeInvalidRequest, PhaseSnapshot, "snapshot time is outside the recorder range")
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrSnapshotDecoderUnavailable
	}
	stream, err := selectStream(c.Inventory(), &Source{Camera: cameraID})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	input, err := snapshotSource(ctx, c, stream, at)
	if err != nil {
		return nil, err
	}
	_ = c.Close() // Stop live traffic before starting the image decoder.
	return decodeSnapshot(ctx, path, input, stream.Format, width)
}

type snapshotInput struct {
	annexB     []byte
	index      int // FFmpeg output (display order) index of the selected picture
	capturedAt time.Time
}

// The recorder archives footage a few seconds late and its webhooks arrive about
// as late, so a snapshot of a just-happened event polls the recorded edge (as the
// Milestone source does) rather than failing on the first empty search.
const recordedEdgeWait = 12 * time.Second

var (
	edgePollInterval = 500 * time.Millisecond
	timeNow          = time.Now
)

func snapshotSource(ctx context.Context, session mediaSession, stream Stream, at *time.Time) (*snapshotInput, error) {
	if at != nil {
		end := at.Add(3 * time.Second)
		for {
			frames, err := session.Search(ctx, stream.ID, *at, end)
			if err != nil {
				return nil, err
			}
			input, err := selectSnapshot(frames, stream.Format, *at, end)
			if !errors.Is(err, ErrNoSnapshot) || timeNow().After(end.Add(recordedEdgeWait)) {
				return input, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(edgePollInterval):
			}
		}
	}
	if err := session.StartLive(stream.ID); err != nil {
		return nil, err
	}
	// The recorder's buffered GOP lets the current picture be decoded now
	// rather than after the next keyframe (see liveStart).
	state := videoState{format: stream.Format, keepUntimed: true}
	var start liveStart
	var gop snapshotGOP
	interval := nominalInterval(&state, stream)
	bytesRead := 0
	for frames := 0; frames < 3000; frames++ {
		packet, err := session.ReadMedia(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ErrNoSnapshot
			}
			return nil, err
		}
		if packet.StreamID != stream.WireID {
			return nil, errorf(CodeProtocolError, PhaseSnapshot, "unexpected live snapshot stream")
		}
		bytesRead += len(packet.Data)
		if bytesRead > 32<<20 {
			return nil, errorf(CodeProtocolError, PhaseSnapshot, "snapshot video limit exceeded")
		}
		frame, err := state.prepare(packet)
		if err != nil {
			return nil, err
		}
		if frame == nil {
			continue
		}
		for _, f := range start.push(frame, interval) {
			if err = gop.add(f); err != nil {
				return nil, err
			}
			if !f.backdated && gop.selectLast(f) {
				return &gop.input, nil
			}
		}
	}
	return nil, ErrNoSnapshot
}

// selectSnapshot picks the first picture at or after start, feeding FFmpeg
// its GOP in decode order up to that picture.
func selectSnapshot(packets []*MediaPacket, format uint32, start, end time.Time) (*snapshotInput, error) {
	if len(packets) > maxSearchFrames {
		return nil, errorf(CodeProtocolError, PhaseSnapshot, "snapshot frame limit exceeded")
	}
	state := videoState{format: format}
	var gop snapshotGOP
	bytesRead := 0
	for _, packet := range packets {
		bytesRead += len(packet.Data)
		if bytesRead > 32<<20 {
			return nil, errorf(CodeProtocolError, PhaseSnapshot, "snapshot video limit exceeded")
		}
		frame, err := state.prepare(packet)
		if err != nil {
			return nil, err
		}
		if frame == nil {
			continue
		}
		if err = gop.add(frame); err != nil {
			return nil, err
		}
		if frame.pic.B {
			continue
		}
		if frame.time.After(end) {
			break
		}
		if !frame.time.Before(start) && gop.selectLast(frame) {
			return &gop.input, nil
		}
	}
	return nil, ErrNoSnapshot
}

// snapshotGOP accumulates the current GOP in decode order for FFmpeg. In
// B-frame GOPs only I/P pictures are selectable: their display rank follows
// from POC, whereas the recorder's decode-order timestamps cannot be attached
// to B pictures, so the chosen time is approximate by up to one mini-GOP.
type snapshotGOP struct {
	input    snapshotInput
	pictures []reorder.Picture
}

func (g *snapshotGOP) add(f *videoFrame) error {
	if f.key {
		g.input.annexB, g.pictures = nil, g.pictures[:0]
	}
	g.pictures = append(g.pictures, f.pic)
	return appendSnapshotFrame(&g.input, f.data)
}

// selectLast selects the most recently added picture if its display rank is known.
func (g *snapshotGOP) selectLast(f *videoFrame) bool {
	if f.pic.B {
		return false
	}
	index, ok := displayIndex(g.pictures)
	if !ok {
		return false // unreadable picture order in a reordered GOP
	}
	g.input.index, g.input.capturedAt = index, f.time
	return true
}

// displayIndex is the last fed picture's rank in FFmpeg's display order.
func displayIndex(gop []reorder.Picture) (int, bool) {
	selected := gop[len(gop)-1]
	reordered := false
	for _, pic := range gop {
		reordered = reordered || pic.B
	}
	if !reordered {
		return len(gop) - 1, true
	}
	index := 0
	for _, pic := range gop[:len(gop)-1] {
		if !pic.OK || !selected.OK {
			return 0, false
		}
		if pic.POC < selected.POC {
			index++
		}
	}
	return index, true
}

func appendSnapshotFrame(input *snapshotInput, avcc []byte) error {
	if len(input.annexB)+len(avcc) > 32<<20 {
		return errors.New("exacq: snapshot GOP exceeds size limit")
	}
	for len(avcc) > 0 {
		if len(avcc) < 4 {
			return errors.New("exacq: truncated snapshot NAL unit")
		}
		n := int(binary.BigEndian.Uint32(avcc[:4]))
		if n == 0 || n > len(avcc)-4 {
			return errors.New("exacq: invalid snapshot NAL length")
		}
		input.annexB = append(input.annexB, 0, 0, 0, 1)
		input.annexB = append(input.annexB, avcc[4:4+n]...)
		avcc = avcc[4+n:]
	}
	return nil
}

type snapshotOutput struct{ bytes.Buffer }

func (output *snapshotOutput) Write(data []byte) (int, error) {
	if len(data) > 8<<20-output.Len() {
		return 0, errors.New("exacq: JPEG exceeds size limit")
	}
	return output.Buffer.Write(data)
}

func decodeSnapshot(ctx context.Context, path string, input *snapshotInput, format uint32, width int) (*Snapshot, error) {
	if width == 0 {
		width = 4096
	}
	codec := "h264"
	if format == 6 {
		codec = "hevc"
	} else if format != 5 {
		return nil, errorf(CodeUnsupportedCodec, PhaseSnapshot, "snapshots support H.264 and H.265 only (stream format %d)", format)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	filter := fmt.Sprintf(`select=eq(n\,%d),scale=w=min(iw\,%d):h=-2`, input.index, width)
	command := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1", "-f", codec, "-i", "pipe:0", "-map", "0:v:0", "-an", "-vf", filter, "-fps_mode", "vfr", "-frames:v", "1", "-c:v", "mjpeg", "-threads", "1", "-f", "image2pipe", "pipe:1")
	command.Stdin = bytes.NewReader(input.annexB)
	output := &snapshotOutput{}
	command.Stdout = output
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errorf(CodeUnsupportedCodec, PhaseSnapshot, "FFmpeg could not decode the selected picture")
	}
	data := output.Bytes()
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Width > 4096 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 64<<20 {
		return nil, errors.New("exacq: invalid decoded snapshot")
	}
	return &Snapshot{JPEG: data, CapturedAt: input.capturedAt, Width: config.Width, Height: config.Height}, nil
}
