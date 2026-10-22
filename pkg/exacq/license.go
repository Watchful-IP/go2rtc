package exacq

import (
	"context"
	"errors"
	"math"
	"time"
)

// License is a read-only, non-secret view of the recorder's license health.
// Status is invalid, expired, unlicensed, trial, restricted, or licensed.
// Incomplete replies return an error rather than an assumed licensed state.
type License struct {
	Status        string     `json:"status"`
	Enterprise    bool       `json:"enterprise"`
	IPCameraSlots uint32     `json:"ipCameraSlots"`
	ExpiresAt     *time.Time `json:"expiresAt"`
}

// licenseUsable rejects licence states in which the recorder serves no video.
func licenseUsable(license *License) error {
	switch license.Status {
	case "invalid", "expired", "unlicensed":
		return errorf(CodeStreamUnavailable, PhaseLicense, "recorder licence is %s; video is unavailable until it is licensed", license.Status)
	}
	return nil
}

// ReadLicense requests 9/1 license information followed by 3/1 status. It does
// not change or install a license. Use a dedicated control connection.
func (c *Client) ReadLicense(ctx context.Context) (license *License, err error) {
	defer func() { err = classify(err, PhaseLicense) }()
	timeout := c.options.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.sendPacket(packetLicense, 1, nil); err != nil {
		return nil, err
	}
	hasKey, receivedKey := false, false
	var controlBytes int
	for i := 0; i < 128; i++ {
		p, err := c.readNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		controlBytes += len(p.Payload)
		if controlBytes > maxDecodedSize {
			return nil, errors.New("exacq: license response size limit exceeded")
		}
		if !receivedKey && p.Type == packetLicense && p.Subtype == 2 {
			if p.Status != 0 {
				return nil, withStatus(errorf(CodeProtocolError, PhaseLicense, "recorder rejected the licence query (status %d)", int(int16(p.Status))), p.Status)
			}
			hasKey, err = licenseKeyPresent(p.Payload)
			// The response contains a licensed MAC and key. Neither is retained.
			clear(p.Payload)
			if err != nil {
				return nil, err
			}
			receivedKey = true
			if err = c.sendPacket(packetStatus, 1, nil); err != nil {
				return nil, err
			}
			continue
		}
		if receivedKey && p.Type == packetStatus && p.Subtype == 2 {
			if p.Status != 0 {
				return nil, withStatus(errorf(CodeProtocolError, PhaseLicense, "recorder rejected the licence status request (status %d)", int(int16(p.Status))), p.Status)
			}
			root, parseErr := parseConfigXML(p.Payload)
			if parseErr != nil {
				return nil, parseErr
			}
			// Status updates share 3/2 with full requested status, and the
			// observed partial updates carry no Delta attribute. Wait for the
			// license-bearing reply instead of classifying an unrelated update.
			if len(root.child("Systems").children("License")) == 0 {
				continue
			}
			return parseLicenseRoot(root, hasKey)
		}
	}
	return nil, errors.New("exacq: license response limit exceeded")
}

func licenseKeyPresent(payload []byte) (bool, error) {
	// Native 9/2 is six MAC bytes followed by a 16-byte binary license key.
	if len(payload) != 22 {
		return false, errors.New("exacq: unsupported license response length")
	}
	var present byte
	for _, b := range payload[6:] {
		present |= b
	}
	return present != 0, nil
}

func parseLicense(data []byte, hasKey bool) (*License, error) {
	root, err := parseConfigXML(data)
	if err != nil {
		return nil, err
	}
	return parseLicenseRoot(root, hasKey)
}

func parseLicenseRoot(root *configNode, hasKey bool) (*License, error) {
	nodes := root.child("Systems").children("License")
	if len(nodes) != 1 {
		return nil, errors.New("exacq: missing or ambiguous license status")
	}
	node := nodes[0]
	for _, name := range []string{"IsBadLicense", "Trial", "IsRestricted", "Enterprise", "MaxIPCameras", "Expiration", "Time"} {
		if !node.has(name) {
			if !hasKey && emptyLicenseStatus(node) {
				return &License{Status: "unlicensed"}, nil
			}
			return nil, errors.New("exacq: incomplete license status")
		}
	}
	p := configParser{}
	invalid := p.boolean(node, "IsBadLicense", false)
	trial := p.boolean(node, "Trial", false)
	restricted := p.boolean(node, "IsRestricted", false)
	enterprise := p.boolean(node, "Enterprise", false)
	enterpriseEx := p.boolean(node, "EnterpriseEx", false)
	slots := p.number(node, "MaxIPCameras", 65536, true)
	expiry := p.number(node, "Expiration", math.MaxInt32, true)
	serverTime := p.number(node, "Time", math.MaxInt32, true)
	if p.err != nil {
		return nil, p.err
	}
	license := &License{Enterprise: hasKey && (enterprise || enterpriseEx), IPCameraSlots: slots}
	expired := hasKey && serverTime >= expiry
	if expiry > 0 && !(trial && expired) {
		stamp := time.Unix(int64(expiry), 0).UTC()
		license.ExpiresAt = &stamp
	}
	// The SDK clears an expired trial's expiration value and gives invalid
	// and expired states priority over its remaining license flags.
	switch {
	case invalid:
		license.Status = "invalid"
	case expired:
		license.Status = "expired"
	case !hasKey:
		license.Status = "unlicensed"
	case trial:
		license.Status = "trial"
	case restricted:
		license.Status = "restricted"
	default:
		license.Status = "licensed"
	}
	return license, nil
}

// A fresh unlicensed recorder omits its license flags and clock entirely.
// Accept only that observed empty shape; a keyed reply still requires all flags.
func emptyLicenseStatus(node *configNode) bool {
	for _, name := range []string{"IsRestricted", "MaxIPCameras", "TotalIPCameras", "AnalogCameras", "Expiration"} {
		if !node.has(name) || node.attr(name) != "0" {
			return false
		}
	}
	for _, name := range []string{"IsBadLicense", "Trial", "Enterprise", "EnterpriseEx", "Time"} {
		if node.has(name) {
			return false
		}
	}
	return true
}
