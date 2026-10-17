// Package infra classifies dependency failures and provides bounded retries.
package infra

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math/rand"
	"net"
	"os/exec"
	"strings"
	"time"
)

type Dep string

const (
	Claude Dep = "Claude"
	Codex  Dep = "Codex"
	GitHub Dep = "GitHub"
	Orca   Dep = "Orca"
)

// Error strings deliberately omit underlying service messages and credentials.
type Transient struct{ Err error }

func (e *Transient) Error() string { return "transient dependency failure" }
func (e *Transient) Unwrap() error { return e.Err }

type Ticket struct{ Err error }

func (e *Ticket) Error() string { return "ticket failure" }
func (e *Ticket) Unwrap() error { return e.Err }

// Reason is a safe, log-friendly category of a Global failure. Only
// RuntimeUnavailable is expected to clear without operator action.
type Reason string

const (
	RuntimeUnavailable   Reason = "runtime_unavailable"
	CredentialsMissing   Reason = "credentials_missing"
	AuthenticationFailed Reason = "authentication_failed"
	PermissionDenied     Reason = "permission_denied"
	ExecutableMissing    Reason = "executable_missing"
)

type Global struct {
	Dep    Dep
	Reason Reason
	Err    error
}

func (e *Global) Error() string { return "global dependency failure" }
func (e *Global) Unwrap() error { return e.Err }

// HTTPError lets adapters classify HTTP responses without retaining their bodies.
type HTTPError struct{ StatusCode int }

func (e HTTPError) Error() string { return "HTTP request failed" }

var ErrWriteUnconfirmed = errors.New("write not confirmed")

// Classify preserves explicit classifications. Unknown errors affect one ticket;
// adapters mark dependency-wide outages explicitly using Global.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	var global *Global
	var ticket *Ticket
	var transient *Transient
	if errors.As(err, &global) || errors.As(err, &ticket) || errors.As(err, &transient) {
		return err
	}
	var network net.Error
	var record tls.RecordHeaderError
	var certificate *tls.CertificateVerificationError
	var http HTTPError
	var httpPtr *HTTPError
	retryable := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrWriteUnconfirmed)
	retryable = retryable || (errors.As(err, &network) && network.Timeout()) || errors.As(err, &record) || errors.As(err, &certificate)
	retryable = retryable || (errors.As(err, &http) && http.StatusCode >= 500 && http.StatusCode <= 599) || (errors.As(err, &httpPtr) && httpPtr.StatusCode >= 500 && httpPtr.StatusCode <= 599)
	var exit *exec.ExitError
	retryable = retryable || (errors.As(err, &exit) && networkStderr(string(exit.Stderr)))
	if retryable {
		return &Transient{Err: err}
	}
	return &Ticket{Err: err}
}

// networkStderr recognises connection-level failures reported by git, gh and
// curl on stderr. HTTP status errors (403, 404) are deliberately excluded.
func networkStderr(stderr string) bool {
	for _, marker := range []string{"GnuTLS", "handshake failed", "TLS connection", "Could not resolve host", "Connection timed out", "Operation timed out", "Connection reset", "Failed to connect", "early EOF", "hung up unexpectedly", "i/o timeout"} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

// Options injects timing and read-back behavior. Non-idempotent operations require
// ReadBack before replay: true means success, false permits replay, an error
// stops replay because the write's outcome remains unknown.
type Options struct {
	Idempotent bool
	ReadBack   func(context.Context) (bool, error)
	Sleep      func(context.Context, time.Duration) error
	Random     func() float64
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Retry attempts op once, then at most three more times with 2s/4s/8s delays
// and uniform ±10% jitter. Exhausted transient failures become Ticket errors.
func Retry(ctx context.Context, options Options, op func(context.Context) error) error {
	pause := options.Sleep
	if pause == nil {
		pause = sleep
	}
	random := options.Random
	if random == nil {
		random = rand.Float64
	}
	var last error
	for attempt := 0; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			base := time.Second * time.Duration(1<<attempt)
			if err := pause(ctx, time.Duration(float64(base)*(0.9+0.2*random()))); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !options.Idempotent {
				ok, err := options.ReadBack(ctx)
				if err != nil {
					return &Ticket{Err: err}
				}
				if ok {
					return nil
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		last = Classify(op(ctx))
		if last == nil {
			return nil
		}
		var global *Global
		var ticket *Ticket
		var transient *Transient
		if errors.As(last, &global) || errors.As(last, &ticket) || !errors.As(last, &transient) {
			return last
		}
		if !options.Idempotent && options.ReadBack == nil {
			return &Ticket{Err: last}
		}
	}
	return &Ticket{Err: last}
}
