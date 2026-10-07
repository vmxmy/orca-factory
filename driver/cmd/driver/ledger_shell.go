package main

import (
	"os"

	"github.com/vmxmy/orca-factory/driver/internal/ledger"
)

func init() { Register("ledger-shell", runLedgerShell) }

func runLedgerShell(args []string) int {
	return ledger.RunShell(args, os.Stdout, os.Stderr)
}
