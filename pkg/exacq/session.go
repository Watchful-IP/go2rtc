package exacq

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"
)

// Connect authenticates and reads the recorder configuration. The caller owns
// the returned session and must close it, including after a failed operation.
func Connect(ctx context.Context, options Options) (*Client, error) {
	if options.Timeout <= 0 {
		options.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	c, err := openTransport(ctx, options)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	err = c.login(ctx)
	options.Trace.Observe(PhaseLogin, start)
	if err != nil {
		return nil, classify(err, PhaseLogin)
	}
	start = time.Now()
	c.inventory, err = c.readInventory(ctx)
	options.Trace.Observe(PhaseConfiguration, start)
	if err != nil {
		return nil, classify(err, PhaseConfiguration)
	}
	options.Trace.identify(c.inventory.Server.Serial, c.inventory.Server.Version)
	go c.keepalive()
	return c, nil
}

// Recorder identifies the authenticated recorder: discovery serial and the
// configured product version, when the recorder reports them.
func (c *Client) Recorder() (serial, version string) {
	serial = c.discovery.Serial
	if inv := c.Inventory(); inv != nil {
		if inv.Server.Serial != "" {
			serial = inv.Server.Serial
		}
		version = inv.Server.Version
	}
	return serial, version
}

// readInventory requires a fresh session. Delta broadcasts cannot stand in for
// the requested full configuration, and unrelated traffic cannot extend its
// deadline or byte budget. Failed reads close the partially consumed session.
func (c *Client) readInventory(ctx context.Context) (inventory *Inventory, err error) {
	timeout := c.options.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = c.Close()
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = c.sendPacket(packetConfig, 1, nil); err != nil {
		return nil, err
	}
	controlBytes := 0
	for count := 0; count < 128; count++ {
		var p *packet
		p, err = c.readNext(ctx)
		if err != nil {
			return nil, err
		}
		controlBytes += len(p.Payload)
		if controlBytes > maxDecodedSize {
			return nil, errors.New("exacq: configuration response byte limit exceeded")
		}
		if p.Type != packetConfig || p.Subtype != 2 {
			continue
		}
		if p.Status != 0 {
			return nil, withStatus(errorf(CodePermissionDenied, PhaseConfiguration, "recorder refused to send its configuration (status %d); the account may lack configuration access", int(int16(p.Status))), p.Status)
		}
		root, parseErr := parseConfigXML(p.Payload)
		if parseErr != nil {
			return nil, parseErr
		}
		parser := configParser{}
		delta := parser.boolean(root, "Delta", false)
		if parser.err != nil {
			return nil, parser.err
		}
		if delta {
			continue
		}
		inventory, err = parseConfigRoot(root)
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		return inventory, nil
	}
	return nil, errors.New("exacq: configuration message limit exceeded")
}

// Inventory returns an immutable snapshot from the authenticated session.
// Callers must not mutate it. Reconnect to refresh the complete inventory.
func (c *Client) Inventory() *Inventory {
	c.inventoryMu.RLock()
	defer c.inventoryMu.RUnlock()
	return c.inventory
}

func (c *Client) keepalive() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.sendPacket(packetDiscover, 1, nil); err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

// readNext handles transport rekeys inside an established session. There is
// one reader per session; the public blocking operations serialize ownership.
func (c *Client) readNext(ctx context.Context) (*packet, error) {
	for count := 0; count < 32; count++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(c.options.Timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = c.conn.SetReadDeadline(deadline)
		p, err := c.readPacket()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if p.Type != packetKey {
			return p, nil
		}
		switch p.Subtype {
		case 1:
			if len(c.pendingKey) != 0 {
				return nil, errors.New("exacq: overlapping rekey challenge")
			}
			var reply []byte
			reply, c.pendingKey, err = legacyDHResponse(p.Payload, rand.Reader)
			if err != nil {
				return nil, err
			}
			// The SDK activates TX before writing the plain DH reply; RX waits
			// for acknowledgement. Keep a concurrent heartbeat behind that reply.
			c.writeMu.Lock()
			c.txKey = c.pendingKey
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.options.Timeout))
			err = writePacket(c.conn, &packet{Type: packetKey, Subtype: 2, Payload: reply})
			c.writeMu.Unlock()
			if err != nil {
				return nil, err
			}
		case 3:
			if len(c.pendingKey) == 0 {
				return nil, errors.New("exacq: unexpected rekey acknowledgement")
			}
			c.rxKey = c.pendingKey
			c.pendingKey = nil
		default:
			return nil, errors.New("exacq: unsupported rekey packet")
		}
	}
	return nil, errors.New("exacq: rekey message limit exceeded")
}

// StartLive requests one native camera/stream CASID on this connection.
func (c *Client) StartLive(camera uint32) error {
	if camera == 0 {
		return errorf(CodeInvalidRequest, PhaseLive, "live camera is required")
	}
	return classify(c.sendPacket(packetLive, 1, liveRequest([]uint32{camera}, true)), PhaseLive)
}

// ReadMedia waits for the next live frame. Cancellation closes the session so
// an interrupted read cannot desynchronize its next operation.
func (c *Client) ReadMedia(ctx context.Context) (frame *MediaPacket, err error) {
	defer func() { err = classify(err, PhaseLive) }()
	ctx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()
	c.readMu.Lock()
	defer c.readMu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	controlBytes := 0
	for count := 0; count < 128; count++ {
		p, err := c.readNext(ctx)
		if err != nil {
			return nil, err
		}
		if p.Type == packetLive {
			if p.Status != 0 {
				return nil, withStatus(errorf(CodeStreamUnavailable, PhaseLive, "recorder rejected the live stream request (status %d); check the camera is connected and the user may view it", int(int16(p.Status))), p.Status)
			}
			if p.Subtype == 3 {
				return parseMediaPacket(p.Payload)
			}
		}
		controlBytes += len(p.Payload)
		if controlBytes > maxDecodedSize {
			return nil, errors.New("exacq: live control byte limit exceeded")
		}
	}
	return nil, errors.New("exacq: live control message limit exceeded")
}

const maxSearchFrames = 3000
const maxSearchBytes = 32 << 20

// Search fetches one finite segment, including native preroll. It does not pace
// frames or remove recording gaps. The producer schedules successive segments.
func (c *Client) Search(ctx context.Context, camera uint32, start, end time.Time) (frames []*MediaPacket, err error) {
	defer func() { err = classify(err, PhaseSearch) }()
	if camera == 0 || start.Unix() < 0 || !end.After(start) || end.Sub(start) > time.Minute {
		return nil, errorf(CodeInvalidRequest, PhaseSearch, "invalid search segment")
	}
	// Seconds are inclusive in the native query; retain subsecond boundary
	// frames and let the caller deduplicate/filter the requested interval.
	if end.Nanosecond() != 0 {
		end = end.Truncate(time.Second).Add(time.Second)
	}
	if end.Unix() > math.MaxInt32 {
		return nil, errorf(CodeInvalidRequest, PhaseSearch, "search time exceeds supported recorder range")
	}
	ctx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()
	c.readMu.Lock()
	defer c.readMu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	if err := c.sendPacket(packetStored, 1, searchRequest(camera, start, end)); err != nil {
		return nil, err
	}
	var expected, received uint64
	var metadata bool
	var size int
	streams := map[uint32]bool{}
	controlCount, controlBytes := 0, 0
	for packets := 0; packets < maxSearchFrames+4096; packets++ {
		p, err := c.readNext(ctx)
		if err != nil {
			return nil, err
		}
		if p.Type != packetStored {
			controlCount++
			controlBytes += len(p.Payload)
			if controlCount > 128 || controlBytes > maxDecodedSize {
				return nil, errors.New("exacq: search control limit exceeded")
			}
			continue
		}
		if p.Status != 0 {
			return nil, searchRejected(p.Status)
		}
		switch p.Subtype {
		case 2:
			if metadata {
				return nil, errors.New("exacq: duplicate search metadata")
			}
			results, err := parseSearch(p.Payload)
			if err != nil {
				return nil, err
			}
			for _, result := range results {
				if result.Camera != camera {
					return nil, errors.New("exacq: unexpected camera in search response")
				}
				expected += result.Count
				if expected > maxSearchFrames {
					return nil, errorf(CodeInvalidRequest, PhaseSearch, "recording segment has more than %d frames; request a shorter interval", maxSearchFrames)
				}
				streams[result.StreamID] = true
			}
			metadata = true
			if expected == 0 {
				return nil, nil
			}
		case 3:
			// 26.1 emits this empty control record after the advertised media
			// count, including empty searches. It is not part of Count and can
			// remain queued when the next finite segment is requested. Accept
			// only its observed exact shape; it must never establish media EOF.
			if isStoredBoundary(p.Payload) {
				continue
			}
			if !metadata {
				return nil, errors.New("exacq: media precedes search metadata")
			}
			frame, err := parseMediaPacket(p.Payload)
			if err != nil {
				return nil, err
			}
			if !streams[frame.StreamID] {
				return nil, errors.New("exacq: unknown search stream")
			}
			received++
			size += len(p.Payload)
			if size > maxSearchBytes {
				return nil, errors.New("exacq: search byte limit exceeded")
			}
			frames = append(frames, frame)
			if received == expected {
				return frames, nil
			}
		default:
			return nil, fmt.Errorf("exacq: unexpected search subtype %d", p.Subtype)
		}
	}
	return nil, errors.New("exacq: search message limit exceeded")
}

func isStoredBoundary(payload []byte) bool {
	return len(payload) == streamHeaderSize && bytes.Equal(payload, []byte{
		0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	})
}
