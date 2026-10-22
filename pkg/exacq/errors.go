package exacq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
)

// Code is a stable, machine-readable failure class shared with the ihub
// adapter. Messages are for people; callers branch on Code.
type Code string

const (
	CodeInvalidRequest         Code = "invalid_request"
	CodeUnreachable            Code = "unreachable"
	CodeProxyFailed            Code = "proxy_failed"
	CodeTimeout                Code = "timeout"
	CodeNotExacq               Code = "not_exacq"
	CodeProtocolError          Code = "protocol_error"
	CodeConnectionLost         Code = "connection_lost"
	CodeAuthInvalidCredentials Code = "auth_invalid_credentials"
	CodeAuthNoAccounts         Code = "auth_no_accounts"
	CodeAuthScheduleBlocked    Code = "auth_schedule_blocked"
	CodeAuthDirectoryFailed    Code = "auth_directory_failed"
	CodeAuthUnsupported        Code = "auth_unsupported"
	CodeAuthRejected           Code = "auth_rejected"
	CodePermissionDenied       Code = "permission_denied"
	CodeCameraNotFound         Code = "camera_not_found"
	CodeStreamUnavailable      Code = "stream_unavailable"
	CodeUnsupportedCodec       Code = "unsupported_codec"
	CodeNoRecording            Code = "no_recording"
	CodeWebhookUnsupported     Code = "webhook_unsupported"
	CodeConfigConflict         Code = "config_conflict"
	CodeConfigRejected         Code = "config_rejected"
	CodeConfigNotPersisted     Code = "config_not_persisted"
	CodeBusy                   Code = "busy"
	CodeDecoderUnavailable     Code = "decoder_unavailable"
	CodeInternal               Code = "internal"
)

const (
	PhaseDial          = "dial"
	PhaseHandshake     = "handshake"
	PhaseLogin         = "login"
	PhaseConfiguration = "configuration"
	PhaseStatus        = "status"
	PhaseLicense       = "license"
	PhaseLive          = "live"
	PhaseSearch        = "search"
	PhaseSnapshot      = "snapshot"
	PhaseTalkdown      = "talkdown"
	PhaseWebhook       = "webhook"
	PhaseMotion        = "motion"
	PhaseRequest       = "request"
)

// Error is a classified failure. Message is secret-free and actionable;
// RecorderStatus is the signed native status when the recorder supplied one.
type Error struct {
	Code           Code
	Phase          string
	Message        string
	Retryable      bool
	RecorderStatus *int
	Err            error
}

func (e *Error) Error() string { return "exacq: " + e.Message }
func (e *Error) Unwrap() error { return e.Err }

func retryable(code Code) bool {
	switch code {
	case CodeUnreachable, CodeProxyFailed, CodeTimeout, CodeConnectionLost, CodeConfigNotPersisted, CodeBusy:
		return true
	}
	return false
}

func errorf(code Code, phase, format string, args ...any) *Error {
	return &Error{Code: code, Phase: phase, Message: fmt.Sprintf(format, args...), Retryable: retryable(code)}
}

// wrapf classifies with an underlying cause retained for errors.Is/As.
func wrapf(err error, code Code, phase, format string, args ...any) *Error {
	e := errorf(code, phase, format, args...)
	e.Err = err
	return e
}

func withStatus(e *Error, status uint16) *Error {
	signed := int(int16(status))
	e.RecorderStatus = &signed
	return e
}

// AsError returns the classified error in err's chain, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// classify gives an unclassified failure during phase a code. Typed errors
// pass through; transport conditions are recognised; anything else read from
// the recorder is a protocol error.
func classify(err error, phase string) error {
	if err == nil {
		return nil
	}
	if e := AsError(err); e != nil {
		if e.Phase == "" {
			e.Phase = phase
		}
		return err
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return wrapf(err, CodeTimeout, phase, "recorder did not respond in time during %s", phase)
	case errors.Is(err, context.Canceled):
		return wrapf(err, CodeTimeout, phase, "request was cancelled during %s", phase)
	case connectionLost(err):
		return wrapf(err, CodeConnectionLost, phase, "recorder closed the connection during %s", phase)
	}
	return wrapf(err, CodeProtocolError, phase, "unexpected recorder response during %s: %s", phase, strings.TrimPrefix(err.Error(), "exacq: "))
}

func connectionLost(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED)
}

// dialError explains a failed TCP or SOCKS dial. Proxy userinfo never appears:
// only the recorder address and the SOCKS reply text are reported.
func dialError(err error, address string, proxied bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if proxied {
			return wrapf(err, CodeTimeout, PhaseDial, "timed out connecting to %s through EdgeLink; check the site bridge is online and can reach the recorder", address)
		}
		return wrapf(err, CodeTimeout, PhaseDial, "timed out connecting to %s; check the address, port forwarding and firewall", address)
	}
	var op *net.OpError
	if proxied && errors.As(err, &op) && strings.HasPrefix(op.Op, "socks") {
		inner := op.Err
		var dial *net.OpError
		switch {
		case errors.As(inner, &dial) && dial.Op == "dial":
			return wrapf(err, CodeProxyFailed, PhaseDial, "could not reach the EdgeLink proxy: %s", plainDialReason(inner))
		case strings.HasPrefix(inner.Error(), "unknown error "):
			return wrapf(err, CodeUnreachable, PhaseDial, "EdgeLink could not reach the recorder at %s (%s)", address, strings.TrimPrefix(inner.Error(), "unknown error "))
		case connectionLost(inner):
			return wrapf(err, CodeProxyFailed, PhaseDial, "EdgeLink proxy closed the connection before routing to %s", address)
		default:
			return wrapf(err, CodeProxyFailed, PhaseDial, "EdgeLink proxy refused the route to %s (%s)", address, inner.Error())
		}
	}
	if proxied {
		return wrapf(err, CodeProxyFailed, PhaseDial, "could not reach the EdgeLink proxy: %s", plainDialReason(err))
	}
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns):
		return wrapf(err, CodeUnreachable, PhaseDial, "could not resolve recorder host %q", dns.Name)
	case errors.Is(err, syscall.ECONNREFUSED):
		return wrapf(err, CodeUnreachable, PhaseDial, "connection refused by %s; the exacqVision Server service may be stopped or the port blocked", address)
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return wrapf(err, CodeUnreachable, PhaseDial, "no network route to %s", address)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return wrapf(err, CodeTimeout, PhaseDial, "timed out connecting to %s; check the address, port forwarding and firewall", address)
	}
	return wrapf(err, CodeUnreachable, PhaseDial, "could not connect to %s: %s", address, plainDialReason(err))
}

func plainDialReason(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return "no route to host"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "host not found"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	var op *net.OpError
	for errors.As(err, &op) && op.Err != nil {
		err = op.Err
	}
	return err.Error()
}

// loginError maps a nonzero login status. Status values are u16 on the wire
// and signed in the SDK enum (docs/exacq/wire-research.md).
func loginError(status uint16) error {
	var e *Error
	switch int(int16(status)) {
	case -3:
		e = errorf(CodeAuthInvalidCredentials, PhaseLogin, "recorder rejected the username or password")
	case -2:
		e = errorf(CodeAuthNoAccounts, PhaseLogin, "recorder has no user accounts configured")
	case -4:
		e = errorf(CodeAuthScheduleBlocked, PhaseLogin, "user is not permitted to log in at this time (blocked by the user's login schedule)")
	case 1, 2, 3:
		e = errorf(CodeAuthDirectoryFailed, PhaseLogin, "recorder's directory (LDAP/Kerberos) login failed")
	case 4, 7:
		e = errorf(CodeAuthUnsupported, PhaseLogin, "recorder requires an unsupported login method (enterprise or OAuth)")
	default:
		e = errorf(CodeAuthRejected, PhaseLogin, "recorder rejected the login (status %d)", int(int16(status)))
	}
	return withStatus(e, status)
}

// configRejected explains a refused configuration write.
func configRejected(phase string, status uint16) error {
	return withStatus(errorf(CodeConfigRejected, phase, "recorder rejected the %s configuration change (status %d); the account may need Setup or administrator permission", phase, int(int16(status))), status)
}

// searchRejected maps stored search statuses: 1 not permitted, 2 not bound,
// 3 start failure, 4 execution failure.
func searchRejected(status uint16) error {
	switch status {
	case 1:
		return withStatus(errorf(CodePermissionDenied, PhaseSearch, "user lacks permission to search recordings for this camera"), status)
	case 2:
		return withStatus(errorf(CodeStreamUnavailable, PhaseSearch, "camera stream is not bound to recorder storage"), status)
	}
	return withStatus(errorf(CodeStreamUnavailable, PhaseSearch, "recorder could not run the recording search (status %d)", int(int16(status))), status)
}
