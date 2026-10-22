package exacq

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// SendAudio sends at most 30 seconds of mono signed 16-bit little-endian PCM to
// an inventory audio output configured with format 0 and the supplied rate.
// The native format 0 is G.711 mu-law on the wire; PCM input is encoded here.
//
// Use a dedicated connected Client. Once validation succeeds this operation
// owns and closes the connection, including on success, cancellation or error.
// Success means the bytes were sent and paced; the protocol provides no proof
// that the physical output reproduced them. No resampling is performed.
func (c *Client) SendAudio(ctx context.Context, outputID uint32, pcm16 []byte, sampleRate int) (err error) {
	defer func() { err = classify(err, PhaseTalkdown) }()
	audio, err := c.audioOutput(outputID, len(pcm16), sampleRate)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	// A reader must remain active while sending: rekeys arrive on this same
	// connection and update the write key under writeMu in readNext.
	readError := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			p, readErr := c.readNext(ctx)
			if readErr != nil {
				readError <- readErr
				return
			}
			if p.Type == packetLive && p.Status != 0 {
				readError <- withStatus(errorf(CodeStreamUnavailable, PhaseTalkdown, "recorder rejected the talkdown audio (status %d); check the output is connected and the user may use audio", int(int16(p.Status))), p.Status)
				return
			}
		}
	}()
	defer func() {
		_ = c.Close()
		<-readerDone
	}()

	samples := int(audio.SamplesPerBuffer)
	started := time.Now()
	for offset := 0; offset < len(pcm16); offset += samples * 2 {
		due := started.Add(time.Duration(offset/2) * time.Second / time.Duration(sampleRate))
		if err = c.waitAudio(ctx, due, readError); err != nil {
			return err
		}
		end := min(offset+samples*2, len(pcm16))
		payload := audioPacket(audio.WireID, pcm16[offset:end], samples, time.Now())
		if err = c.sendPacket(packetLive, 3, payload); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	// The last buffer is padded to the configured size. Keep the connection
	// alive for that full buffer after its write, including a delayed write.
	return c.waitAudio(ctx, time.Now().Add(time.Duration(samples)*time.Second/time.Duration(sampleRate)), readError)
}

func (c *Client) audioOutput(outputID uint32, pcmBytes, sampleRate int) (Audio, error) {
	if outputID == 0 || sampleRate < 8000 || sampleRate > 48000 || pcmBytes == 0 || pcmBytes&1 != 0 || pcmBytes > sampleRate*2*30 {
		return Audio{}, errorf(CodeInvalidRequest, PhaseTalkdown, "audio requires at most 30 seconds of mono signed 16-bit PCM at 8000–48000 Hz")
	}
	inv := c.Inventory()
	if inv == nil {
		return Audio{}, errorf(CodeInternal, PhaseTalkdown, "audio requires authenticated inventory")
	}
	for _, device := range inv.Devices {
		for _, audio := range device.Audio {
			if audio.ID != outputID {
				continue
			}
			// SDK FSendAudio (0x2f7240) requires fOutput, not the separate
			// AudioChannel Enabled state. Output-only devices can report false.
			if !audio.Output || audio.Format == nil || *audio.Format != 0 || audio.SampleRate != uint32(sampleRate) || audio.WireID == 0 {
				return Audio{}, errorf(CodeUnsupportedCodec, PhaseTalkdown, "audio %d is not a G.711 output at %d Hz; check the speaker's audio settings on the recorder", outputID, sampleRate)
			}
			if audio.SamplesPerBuffer == 0 || audio.SamplesPerBuffer > uint64(sampleRate) {
				return Audio{}, errorf(CodeProtocolError, PhaseTalkdown, "recorder reports an invalid audio buffer size for output %d", outputID)
			}
			return audio, nil
		}
	}
	return Audio{}, errorf(CodeCameraNotFound, PhaseTalkdown, "audio output %d is not on this recorder", outputID)
}

func (c *Client) waitAudio(ctx context.Context, due time.Time, readError <-chan error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case err := <-readError:
		return err
	case <-c.done:
		return net.ErrClosed
	default:
	}
	wait := time.Until(due)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-readError:
		return err
	case <-c.done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return net.ErrClosed
	case <-timer.C:
		return nil
	}
}

// audioPacket matches FSendStreamPacket: a zeroed 32-byte media header with
// client wall time, type 0 and the output's configured wire stream ID.
func audioPacket(wireID uint32, pcm16 []byte, samples int, now time.Time) []byte {
	payload := make([]byte, streamHeaderSize+samples)
	binary.LittleEndian.PutUint32(payload, wireID)
	binary.LittleEndian.PutUint32(payload[4:], uint32(now.Unix()))
	binary.LittleEndian.PutUint16(payload[8:], uint16(now.Nanosecond()/int(time.Millisecond)))
	for i := 0; i < samples; i++ {
		var sample int16
		if i*2+1 < len(pcm16) {
			sample = int16(binary.LittleEndian.Uint16(pcm16[i*2:]))
		}
		payload[streamHeaderSize+i] = audioMuLaw(sample)
	}
	return payload
}

// audioMuLaw follows the SDK's G711 conversion, including its one's-complement
// treatment of negative samples and saturation. int32 avoids int16 overflow at
// -32768. All 65,536 input values are checked against the binary oracle in tests.
func audioMuLaw(sample int16) byte {
	value := int32(sample)
	mask := byte(0xef)
	if value < 0 {
		value = ^value
		mask = 0x6f
	}
	value = min(value+132, 0x7f00)
	quantized := value >> 3
	if value > 0x7ff {
		quantized = value >> 7
		mask ^= 0x40
	}
	if quantized > 0x3f {
		quantized >>= 2
		mask ^= 0x20
	}
	if quantized > 0x1f {
		quantized >>= 1
		mask ^= 0x10
	}
	return byte(quantized) ^ mask
}

// audioPayload returns the elementary audio bytes for the configured standard.
// Format 4 carries an MPEG4-generic single-AU header (16 bits of AU headers,
// followed by a 13-bit byte size and 3-bit index), not ADTS.
func audioPayload(format uint8, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, errors.New("exacq: empty audio payload")
	}
	switch format {
	case 0: // G.711 mu-law
		return payload, nil
	case 4: // AAC
		if len(payload) < 4 || payload[0] != 0 || payload[1] != 0x10 {
			return nil, errors.New("exacq: invalid AAC access unit header")
		}
		size := int(binary.BigEndian.Uint16(payload[2:4]) >> 3)
		if size == 0 || size > len(payload)-4 {
			return nil, errors.New("exacq: invalid AAC access unit size")
		}
		return payload[4 : 4+size], nil
	case 6: // signed PCM16 little endian
		if len(payload)&1 != 0 {
			return nil, errors.New("exacq: odd PCM16 payload size")
		}
		return payload, nil
	default:
		return nil, fmt.Errorf("exacq: unsupported audio format %d", format)
	}
}
