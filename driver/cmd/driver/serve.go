package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func init() { Register("serve", serve) }
func serve(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Usage: driver serve")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve home directory")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = scheduler.Serve(ctx, filepath.Join(home, ".local", "state", "orca-factory", "driver-serve.lock"), func(ctx context.Context) (*scheduler.Scheduler, error) {
		cfg, err := config.Load(home)
		if err != nil {
			return nil, err
		}
		runtime, err := scheduler.NewRuntime(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return &scheduler.Scheduler{Config: cfg, Backend: runtime}, nil
	}, func(ctx context.Context) error {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}, func(err error) { fmt.Fprintln(os.Stderr, err) })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
