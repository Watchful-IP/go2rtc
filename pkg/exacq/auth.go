package exacq

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type passwordExchange interface {
	start() []byte
	finish([]byte) ([]byte, []byte, error)
	clear()
}

// login performs the negotiated password-authenticated exchange. Credentials
// are never placed in a cleartext login document or included in errors.
func (c *Client) login(ctx context.Context) (err error) {
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
		return err
	}
	if c.discovery.NegotiateMechanisms&3 == 0 {
		return errorf(CodeAuthUnsupported, PhaseLogin, "recorder offers no supported password login (mechanisms 0x%x); legacy XML or enterprise-only login is not supported", c.discovery.NegotiateMechanisms)
	}
	deadline := time.Now().Add(c.options.Timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err = c.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	var spake passwordExchange
	if c.discovery.NegotiateMechanisms&2 != 0 {
		spake, err = newSpakePlus(c.options.Username, c.options.Password, c.discovery.Serial, nil)
	} else {
		spake, err = newSpake224(c.options.Username, c.options.Password, c.discovery.Serial, nil)
	}
	if err != nil {
		return err
	}
	defer spake.clear()
	if err = c.sendPacket(packetLogin, 5, spake.start()); err != nil {
		return err
	}
	proofSent := false
	for count := 0; count < 16; count++ {
		var p *packet
		p, err = c.readPacket()
		if err != nil {
			return fmt.Errorf("exacq: authenticate: %w", err)
		}
		switch {
		case p.Type == packetLogin && p.Subtype == 5 && !proofSent:
			if p.Status != 0 {
				return loginError(p.Status)
			}
			var response, key []byte
			response, key, err = spake.finish(p.Payload)
			if errors.Is(err, errServerProof) {
				// Both SPAKE variants fail here when the password differs.
				return wrapf(err, CodeAuthInvalidCredentials, PhaseLogin, "recorder rejected the username or password")
			}
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				clear(key)
				return err
			}
			if response != nil {
				if err = c.sendPacket(packetLogin, 5, response); err != nil {
					clear(key)
					return err
				}
			}
			if key != nil {
				c.txKey = key
				c.loginKeyPending = true
				proofSent = true
			}
		case p.Type == packetLogin && p.Subtype == 2:
			if p.Status != 0 {
				return loginError(p.Status)
			}
			if !proofSent || c.loginKeyPending {
				return errors.New("exacq: unexpected authentication acknowledgement")
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			return c.conn.SetDeadline(time.Time{})
		default:
			return fmt.Errorf("exacq: unexpected authentication packet %d/%d", p.Type, p.Subtype)
		}
	}
	return errors.New("exacq: authentication message limit exceeded")
}
