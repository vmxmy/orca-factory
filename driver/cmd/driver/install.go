package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/deploy"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

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
	dryRun := flags.Bool("dry-run", false, "print the action for every installed file without making changes")
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
	units := filepath.Join(home, ".config", "systemd", "user")
	unit := filepath.Join(units, "orca-factory-driver.service")
	gatewayUnit := filepath.Join(units, "orca-factory-feishu-gateway.service")
	watchUnit := filepath.Join(units, "orca-factory-driver-watch.service")
	if err := checkSharedDropIns(unit + ".d"); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return fmt.Errorf("read current binary: %w", err)
	}
	// The binary is replaced without a backup: deploy.sh keeps binary backups.
	files := []installFile{{path: filepath.Join(home, ".local", "bin", "orca-factory-driver"), data: binary, mode: 0755}}
	assets, err := opsAssets(units, filepath.Join(home, ".local", "share", "orca-factory-ops"))
	if err != nil {
		return err
	}
	files = append(files, assets...)
	for i := range files {
		if files[i].action, err = plannedAction(files[i].path, files[i].data); err != nil {
			return err
		}
	}
	if *dryRun {
		for _, file := range files {
			if _, err := fmt.Fprintf(out, "%s %s\n", file.action, file.path); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(out, "Copy existing driver proxy.conf if gateway proxy.conf is absent\nLink %s.d to %s.d\nRun systemctl --user daemon-reload\nRun systemctl --user enable %s\n", watchUnit, unit, watchTimerName)
		return err
	}
	for _, file := range files {
		if err := file.apply(); err != nil {
			return err
		}
	}
	if err := copyGatewayProxy(unit+".d/proxy.conf", gatewayUnit+".d/proxy.conf"); err != nil {
		return err
	}
	if err := linkWatchDropIns(unit+".d", watchUnit+".d"); err != nil {
		return err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", watchTimerName}} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := runner.Run(ctx, runx.Spec{Path: "systemctl", Args: args, Timeout: 30 * time.Second})
		cancel()
		if err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

const watchTimerName = "orca-factory-driver-watch.timer"

type installFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	action string
	backup bool
}

// opsAssets maps the embedded systemd/ tree onto the user unit directory and
// ops/ onto the ops script directory.
func opsAssets(units, ops string) ([]installFile, error) {
	var files []installFile
	err := fs.WalkDir(deploy.Assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := deploy.Assets.ReadFile(name)
		if err != nil {
			return err
		}
		root, rel, _ := strings.Cut(name, "/")
		file := installFile{path: filepath.Join(units, filepath.FromSlash(rel)), data: data, mode: 0644, backup: true}
		if root == "ops" {
			file.path, file.mode = filepath.Join(ops, filepath.FromSlash(rel)), 0755
		}
		files = append(files, file)
		return nil
	})
	return files, err
}

func plannedAction(path string, data []byte) (string, error) {
	current, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "create", nil
	case err != nil:
		return "", err
	case bytes.Equal(current, data):
		return "unchanged", nil
	}
	return "replace", nil
}

func (f installFile) apply() error {
	if f.action == "unchanged" {
		return nil
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if f.action == "replace" && f.backup {
		if err := os.Rename(f.path, f.path+".bak-"+time.Now().UTC().Format("20060102T150405Z")); err != nil {
			return err
		}
	}
	// A sibling temporary file permits replacement even when the installed
	// executable is the currently running binary.
	temp, err := os.CreateTemp(dir, "."+filepath.Base(f.path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }() // Cleanup only; successful rename removes this path.
	if _, err := temp.Write(f.data); err != nil {
		_ = temp.Close() // Preserve the write error; temporary output is discarded.
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := temp.Chmod(f.mode); err != nil {
		_ = temp.Close() // Preserve the chmod error; temporary output is discarded.
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), f.path)
}

func copyGatewayProxy(driverProxy, gatewayProxy string) error {
	if _, err := os.Stat(gatewayProxy); !os.IsNotExist(err) {
		return err
	}
	proxy, err := os.ReadFile(driverProxy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(gatewayProxy), 0755); err != nil {
		return err
	}
	return os.WriteFile(gatewayProxy, proxy, 0600)
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
