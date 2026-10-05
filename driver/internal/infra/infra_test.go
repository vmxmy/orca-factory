package infra

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, context.DeadlineExceeded, tls.RecordHeaderError{}, HTTPError{StatusCode: 503}, ErrWriteUnconfirmed} {
		var transient *Transient
		if !errors.As(Classify(fmt.Errorf("wrapped: %w", err)), &transient) {
			t.Errorf("not transient: %T", err)
		}
	}
	for _, err := range []error{errors.New("invalid ticket"), HTTPError{StatusCode: 404}} {
		var ticket *Ticket
		if !errors.As(Classify(err), &ticket) {
			t.Errorf("not ticket: %v", err)
		}
	}
	for _, dep := range []Dep{Claude, Codex, Linear, GitHub, Orca} {
		original := &Global{Dep: dep, Err: io.EOF}
		var global *Global
		if !errors.As(Classify(original), &global) || global.Dep != dep {
			t.Fatalf("lost global %s", dep)
		}
	}
	ticket := &Ticket{Err: io.EOF}
	if Classify(ticket) != ticket {
		t.Fatal("explicit ticket reclassified")
	}
	if Classify(nil) != nil {
		t.Fatal("nil classified")
	}
}
func TestRetryCountAndJitter(t *testing.T) {
	for _, random := range []float64{0, 0.5, 0.999999} {
		calls := 0
		var sleeps []time.Duration
		err := Retry(context.Background(), Options{Idempotent: true, Random: func() float64 { return random }, Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }}, func(context.Context) error { calls++; return io.EOF })
		var ticket *Ticket
		if !errors.As(err, &ticket) || calls != 4 || len(sleeps) != 3 {
			t.Fatalf("calls=%d sleeps=%v err=%v", calls, sleeps, err)
		}
		for i, base := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
			if sleeps[i] < base*9/10 || sleeps[i] > base*11/10 {
				t.Errorf("delay %v outside jitter", sleeps[i])
			}
		}
	}
}
func TestReadBackPreventsDuplicate(t *testing.T) {
	calls, reads := 0, 0
	err := Retry(context.Background(), Options{ReadBack: func(context.Context) (bool, error) { reads++; return true, nil }, Sleep: func(context.Context, time.Duration) error { return nil }}, func(context.Context) error { calls++; return ErrWriteUnconfirmed })
	if err != nil || calls != 1 || reads != 1 {
		t.Fatalf("calls=%d reads=%d err=%v", calls, reads, err)
	}
}
func TestRetryStopsSafely(t *testing.T) {
	for _, failure := range []error{&Global{Dep: Orca, Err: io.EOF}, &Ticket{Err: io.EOF}} {
		calls := 0
		err := Retry(context.Background(), Options{Idempotent: true}, func(context.Context) error { calls++; return failure })
		if err != failure || calls != 1 {
			t.Fatalf("retried permanent error: %v %d", err, calls)
		}
	}
	calls := 0
	err := Retry(context.Background(), Options{}, func(context.Context) error { calls++; return io.EOF })
	if err == nil || calls != 1 {
		t.Fatal("unsafe non-idempotent retry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = Retry(ctx, Options{Idempotent: true}, func(context.Context) error { t.Fatal("called after cancel"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
func TestReadBackFailureDoesNotRepeatWrite(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), Options{ReadBack: func(context.Context) (bool, error) { return false, io.EOF }, Sleep: func(context.Context, time.Duration) error { return nil }}, func(context.Context) error { calls++; return ErrWriteUnconfirmed })
	if err == nil || calls != 1 {
		t.Fatalf("unsafe replay: %d %v", calls, err)
	}
}
