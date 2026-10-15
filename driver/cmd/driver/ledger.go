package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/ledger"
)

func init() { Register("ledger", runLedger) }

func runLedger(args []string) int {
	appendRow := len(args) == 2 && args[0] == "append"
	stats := len(args) == 1 && args[0] == "stats" || len(args) == 2 && args[0] == "stats" && args[1] == "--all"
	if !appendRow && !stats {
		fmt.Fprintln(os.Stderr, "usage: driver ledger append '<json>' | driver ledger stats [--all]")
		return 2
	}
	env := issueEnvironment()
	if _, err := issues.Backend(env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: home directory unavailable")
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: driver configuration could not be loaded")
		return 1
	}
	tasks, err := issues.New(ctx, env, cfg.Tuning)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	l := ledger.New(&issues.LedgerStore{Client: tasks}, ledger.Options{FlushInterval: cfg.Tuning.Ledger.FlushInterval})
	if appendRow {
		if err = l.Append(args[1]); err != nil {
			_ = l.Close(ctx) // Invalid input was never buffered; preserve the validation error.
			fmt.Fprintln(os.Stderr, "ledger: invalid json row")
			return 2
		}
		// Close synchronously flushes before os.Exit in main.
		err = l.Close(ctx)
	} else {
		err = l.Stats(ctx, len(args) == 2, os.Stdout, os.Stderr)
		closeErr := l.Close(ctx)
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: operation failed")
		return 1
	}
	return 0
}
