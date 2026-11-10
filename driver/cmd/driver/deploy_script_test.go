package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeployFetchRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failures   int
		buildFails bool
		wantEvents string
	}{
		{"first success", 0, false, "fetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nrestart\nsleep 5\nis-active\nresume\nversion\n"},
		{"second success", 1, false, "fetch\nsleep 5\nfetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nrestart\nsleep 5\nis-active\nresume\nversion\n"},
		{"third success", 2, false, "fetch\nsleep 5\nfetch\nsleep 15\nfetch\nmerge\nrev-parse\ndrain\nidle\nbuild\nrestart\nsleep 5\nis-active\nresume\nversion\n"},
		{"exhausted", 3, false, "fetch\nsleep 5\nfetch\nsleep 15\nfetch\n"},
		{"build failure after drain", 0, true, "fetch\nmerge\nrev-parse\ndrain\nidle\nbuild\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local/bin")
			repo := filepath.Join(home, "repo")
			for _, dir := range []string{bin, filepath.Join(repo, "driver")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			scripts := map[string]string{
				"git": `echo "$1" >> "$EVENTS"
case "$1" in
  fetch)
    count=0
    if [ -f "$HOME/fetch-count" ]; then read -r count < "$HOME/fetch-count"; fi
    count=$((count + 1))
    echo "$count" > "$HOME/fetch-count"
    [ "$count" -gt "$FETCH_FAILURES" ] || exit 42
    ;;
  merge) ;;
  rev-parse) echo abc123 ;;
  *) exit 99 ;;
esac
`,
				"sleep":   "echo \"sleep $1\" >> \"$EVENTS\"\n",
				"flock":   "exit 0\n",
				"timeout": "echo idle >> \"$EVENTS\"\n",
				"systemctl": `echo "$2" >> "$EVENTS"
`,
				"go": `echo build >> "$EVENTS"
[ "$BUILD_FAILS" = false ] || exit 43
cp "$HOME/.local/bin/orca-factory-driver" "$HOME/.local/bin/orca-factory-driver.new"
`,
				"orca-factory-driver": `case "$*" in
  "maintenance drain --wait 2m") echo drain >> "$EVENTS"; echo '{"mode":"draining"}' ;;
  "maintenance resume --wait 2m") echo resume >> "$EVENTS"; echo '{"mode":"running"}' ;;
  version) echo version >> "$EVENTS"; echo abc123 ;;
  *) exit 99 ;;
esac
`,
			}
			for name, body := range scripts {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\nset -eu\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			events := filepath.Join(home, "events")
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ORCA_FACTORY_REPO", repo)
			t.Setenv("EVENTS", events)
			t.Setenv("FETCH_FAILURES", strconv.Itoa(tc.failures))
			t.Setenv("BUILD_FAILS", strconv.FormatBool(tc.buildFails))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "bash", "../../deploy/ops/deploy.sh").CombinedOutput()
			wantFailure := tc.failures == 3 || tc.buildFails
			if (err != nil) != wantFailure {
				t.Errorf("exit error = %v, want failure %v; output:\n%s", err, wantFailure, output)
			}
			got, readErr := os.ReadFile(events)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tc.wantEvents {
				t.Errorf("events:\n%s\nwant:\n%s", got, tc.wantEvents)
			}
			if gotMessage := strings.Contains(string(output), "尚未 drain，未改动任何状态"); gotMessage != (tc.failures == 3) {
				t.Errorf("fetch exhaustion message = %v; output:\n%s", gotMessage, output)
			}
			backups, globErr := filepath.Glob(filepath.Join(home, ".local/state/orca-factory/deploy-backups/*/orca-factory-driver"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			wantBackups := 1
			if tc.failures == 3 {
				wantBackups = 0
			}
			if len(backups) != wantBackups {
				t.Errorf("backups = %v, want %d", backups, wantBackups)
			}
		})
	}
}
