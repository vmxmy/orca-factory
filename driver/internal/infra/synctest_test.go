package infra_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

func TestRetryDefaultSleepSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var calls []time.Duration
		err := infra.Retry(context.Background(), infra.Options{
			Idempotent: true,
			Random:     func() float64 { return 0.5 },
		}, func(context.Context) error {
			calls = append(calls, time.Since(start))
			return io.EOF
		})
		want := []time.Duration{0, 2 * time.Second, 6 * time.Second, 14 * time.Second}
		if !slices.Equal(calls, want) {
			t.Errorf("attempt times = %v, want %v (2s/4s/8s delays)", calls, want)
		}
		var ticket *infra.Ticket
		if !errors.As(err, &ticket) || !errors.Is(err, io.EOF) {
			t.Errorf("exhausted retries = %v, want Ticket wrapping EOF", err)
		}
	})
}

func TestRetryDefaultSleepCancellation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    time.Duration
		calls int
	}{
		{"first_wait", time.Second, 1},
		{"second_wait", 3 * time.Second, 2},
		{"third_wait", 7 * time.Second, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				var result error
				done := false
				go func() {
					result = infra.Retry(ctx, infra.Options{
						Idempotent: true,
						Random:     func() float64 { return 0.5 },
					}, func(context.Context) error {
						calls++
						return io.EOF
					})
					done = true
				}()
				// Wait synchronizes observations with the retry goroutine without
				// advancing the bubble clock; Sleep advances virtual time only.
				synctest.Wait()
				time.Sleep(tc.at)
				synctest.Wait()
				if done || calls != tc.calls {
					t.Errorf("before cancel: done=%v calls=%d, want waiting after %d calls", done, calls, tc.calls)
				}
				cancel()
				synctest.Wait()
				if !done || !errors.Is(result, context.Canceled) {
					t.Errorf("cancel must interrupt the pending timer: done=%v err=%v", done, result)
				}
				// Cross every original retry deadline to detect any later call.
				time.Sleep(16 * time.Second)
				synctest.Wait()
				if calls != tc.calls {
					t.Errorf("operation called after cancellation: calls=%d, want %d", calls, tc.calls)
				}
			})
		})
	}
}
