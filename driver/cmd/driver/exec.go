package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/executor"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func init() { Register("exec", execTicket) }
func execTicket(args []string) int {
	repo := executorLogRepository()
	id, merging, valid := "", false, false
	switch {
	case len(args) == 1 && logComponent.MatchString(args[0]):
		id, valid = args[0], true
	case len(args) == 2 && args[0] == "--merging" && logComponent.MatchString(args[1]):
		id, merging, valid = args[1], true, true
	}
	ticket := id
	if !valid {
		ticket = "unknown"
	}
	return loggedCommand("executor", filepath.Join(logStateRoot(), repo, ticket, "executor.log"), os.Stderr, args, func(logger *slog.Logger) (int, error) {
		logger = logger.With("repo", repo, "ticket", ticket)
		if !valid {
			return 2, fmt.Errorf("usage: driver exec [--merging] <票号>")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		result, err := executor.RunTicket(runx.WithLogger(ctx, logger), id, merging)
		if err != nil {
			return 1, err
		}
		logger.Info("ticket completed", "phase", result.State.Phase)
		fmt.Printf("阶段: %s · 归属: %s\n", result.State.Phase, result.State.Owner)
		return 0, nil
	})
}

var logComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Resolve the same profile name used by RunTicket, even for early failures.
func executorLogRepository() string {
	ctx := context.Background()
	b, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"rev-parse", "--show-toplevel"}, Timeout: 10 * time.Second})
	if err != nil {
		return "unknown"
	}
	repo := strings.TrimSpace(string(b))
	b, err = runx.Run(ctx, runx.Spec{Path: "/bin/bash", Args: []string{"-c", `source "$1" >/dev/null || exit; printf '%s' "$AGENT_GH_REPO"`, "driver-log-profile", filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env")}, Dir: repo, Timeout: 10 * time.Second})
	if err == nil {
		_, name, ok := strings.Cut(string(b), "/")
		if ok && logComponent.MatchString(name) {
			return name
		}
	}
	if name := filepath.Base(repo); logComponent.MatchString(name) {
		return name
	}
	return "unknown"
}
