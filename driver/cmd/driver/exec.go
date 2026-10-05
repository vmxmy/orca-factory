package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vmxmy/orca-factory/driver/internal/executor"
)

func init() { Register("exec", execTicket) }
func execTicket(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: driver exec <票号>")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := executor.RunTicket(ctx, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("阶段: %s · 归属: %s\n", result.State.Phase, result.State.Owner)
	return 0
}
