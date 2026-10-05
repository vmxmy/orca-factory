package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Keep this identical to deploy/systemd/orca-factory-driver.service.
// TestInstallWritesUnitAndBinary verifies the installed content against it.
const installUnit = `[Unit]
Description=Orca Factory Driver
After=orca-serve.service network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/orca-factory-driver serve
Environment=AGENT_ORCA_BIN=%h/.local/bin/orca
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=on-failure

[Install]
WantedBy=default.target
`

type installRunner interface {
	Run(context.Context, runx.Spec) ([]byte, error)
}

type installRunx struct{}

func (installRunx) Run(ctx context.Context, spec runx.Spec) ([]byte, error) {
	return runx.Run(ctx, spec)
}

func init() { Register("install", install) }

func install(args []string) int {
	if err := installWith(args, os.Stdout, installRunx{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func installWith(args []string, out io.Writer, runner installRunner) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(out)
	dryRun := flags.Bool("dry-run", false, "print installation steps without making changes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: orca-factory-driver install [--dry-run]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve home directory: %w", err)
	}
	binary := filepath.Join(home, ".local", "bin", "orca-factory-driver")
	unit := filepath.Join(home, ".config", "systemd", "user", "orca-factory-driver.service")
	if *dryRun {
		_, err := fmt.Fprintf(out, "Install current binary to %s\nWrite service unit to %s\nRun systemctl --user daemon-reload\n", binary, unit)
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	source, err := os.Open(executable)
	if err != nil {
		return fmt.Errorf("open current binary: %w", err)
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
		return err
	}
	// A sibling temporary file permits replacement even when the installed
	// executable is the currently running binary.
	temp, err := os.CreateTemp(filepath.Dir(binary), ".orca-factory-driver-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := io.Copy(temp, source); err != nil {
		temp.Close()
		return fmt.Errorf("copy current binary: %w", err)
	}
	if err := temp.Chmod(0755); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), binary); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(unit, []byte(installUnit), 0644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runner.Run(ctx, runx.Spec{Path: "systemctl", Args: []string{"--user", "daemon-reload"}, Timeout: 30 * time.Second}); err != nil {
		return fmt.Errorf("reload user service units: %w", err)
	}
	return nil
}
