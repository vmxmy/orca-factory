package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

const gatewayInstallUnit = `[Unit]
Description=Orca Factory Feishu Gateway
After=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/orca-factory-driver feishu-gateway
EnvironmentFile=-%h/.config/orca-factory/driver.env
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// The watch service reads serve's drop-in directory through a symlink, so
// both always run with the same environment overrides.
const watchInstallUnit = `[Unit]
Description=Orca Factory Driver Watch
After=network-online.target

[Service]
Type=oneshot
ExecStart=%h/.local/bin/orca-factory-driver watch
Environment=AGENT_ORCA_BIN=%h/.local/bin/orca
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
`

const watchInstallTimer = `[Unit]
Description=Run Orca Factory Driver Watch every 30 minutes

[Timer]
OnCalendar=*:0/30
RandomizedDelaySec=5min
Persistent=true

[Install]
WantedBy=timers.target
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
	gatewayUnit := filepath.Join(filepath.Dir(unit), "orca-factory-feishu-gateway.service")
	watchUnit := filepath.Join(filepath.Dir(unit), "orca-factory-driver-watch.service")
	watchTimer := filepath.Join(filepath.Dir(unit), "orca-factory-driver-watch.timer")
	if err := checkSharedDropIns(unit + ".d"); err != nil {
		return err
	}
	if *dryRun {
		_, err := fmt.Fprintf(out, "Install current binary to %s\nWrite service unit to %s\nWrite gateway service unit to %s\nCopy existing driver proxy.conf if gateway proxy.conf is absent\nWrite watch service unit to %s\nWrite watch timer to %s (every 30 minutes, RandomizedDelaySec=5min)\nLink %s.d to %s.d\nRun systemctl --user daemon-reload\n", binary, unit, gatewayUnit, watchUnit, watchTimer, watchUnit, unit)
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
	defer func() { _ = source.Close() }() // Read-only source; copy errors are handled below.
	if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
		return err
	}
	// A sibling temporary file permits replacement even when the installed
	// executable is the currently running binary.
	temp, err := os.CreateTemp(filepath.Dir(binary), ".orca-factory-driver-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }() // Cleanup only; successful rename removes this path.
	if _, err := io.Copy(temp, source); err != nil {
		_ = temp.Close() // Preserve the copy/chmod error; temporary output is discarded.
		return fmt.Errorf("copy current binary: %w", err)
	}
	if err := temp.Chmod(0755); err != nil {
		_ = temp.Close() // Preserve the copy/chmod error; temporary output is discarded.
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
	if err := os.WriteFile(gatewayUnit, []byte(gatewayInstallUnit), 0644); err != nil {
		return err
	}
	gatewayProxy := gatewayUnit + ".d/proxy.conf"
	if _, err := os.Stat(gatewayProxy); os.IsNotExist(err) {
		proxy, readErr := os.ReadFile(unit + ".d/proxy.conf")
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		if readErr == nil {
			if err := os.MkdirAll(filepath.Dir(gatewayProxy), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(gatewayProxy, proxy, 0600); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if err := os.WriteFile(watchUnit, []byte(watchInstallUnit), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(watchTimer, []byte(watchInstallTimer), 0644); err != nil {
		return err
	}
	if err := linkWatchDropIns(unit+".d", watchUnit+".d"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runner.Run(ctx, runx.Spec{Path: "systemctl", Args: []string{"--user", "daemon-reload"}, Timeout: 30 * time.Second}); err != nil {
		return fmt.Errorf("reload user service units: %w", err)
	}
	return nil
}

// checkSharedDropIns refuses serve drop-ins that would change watch's
// behaviour: the directory is shared, so only environment may live there.
func checkSharedDropIns(dir string) error {
	confs, err := filepath.Glob(filepath.Join(dir, "*.conf"))
	if err != nil {
		return err
	}
	for _, conf := range confs {
		raw, err := os.ReadFile(conf)
		if err != nil {
			return err
		}
		section := ""
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			case strings.HasPrefix(line, "["):
				section = strings.Trim(line, "[]")
			default:
				key, _, _ := strings.Cut(line, "=")
				key = strings.TrimSpace(key)
				if section != "Service" || (key != "Environment" && key != "EnvironmentFile") {
					return fmt.Errorf("%s: [%s] %s is not an environment directive; %s is shared with orca-factory-driver-watch.service, so it may only hold [Service] Environment/EnvironmentFile. Move this directive to a separate drop-in outside the shared directory", conf, section, key, dir)
				}
			}
		}
	}
	return nil
}

func linkWatchDropIns(serveDir, watchDir string) error {
	if err := os.MkdirAll(serveDir, 0755); err != nil {
		return err
	}
	target, err := os.Readlink(watchDir)
	switch {
	case err == nil && target == serveDir:
		return nil
	case err == nil || !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s exists and is not a link to %s; move it aside so watch shares serve's drop-ins", watchDir, serveDir)
	}
	return os.Symlink(serveDir, watchDir)
}
