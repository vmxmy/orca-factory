package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

func init() { Register("logs", showLogs) }

func showLogs(args []string) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := flags.Bool("follow", false, "follow new log lines until interrupted")
	asJSON := flags.Bool("json", false, "output original JSON lines")
	since := flags.Duration("since", 0, "show entries from the last duration (for example 30m or 2h)")
	if len(args) == 0 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`).MatchString(args[0]) {
		fmt.Fprintln(os.Stderr, "Usage: driver logs <ticket> [--follow] [--since duration] [--json]")
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *since < 0 {
		fmt.Fprintln(os.Stderr, "logs: invalid arguments")
		return 2
	}
	root := os.Getenv("AGENT_DRIVER_STATE_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	var cutoff time.Time
	if *since > 0 {
		cutoff = time.Now().Add(-*since)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := streamLogs(ctx, root, args[0], cutoff, *follow, *asJSON, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "logs:", err)
		return 1
	}
	return 0
}

type logEntry struct {
	raw       string
	fields    map[string]json.RawMessage
	at        time.Time
	component string
}

// File identity and byte offsets survive rename-based rotation without replaying
// history. Offsets advance only past complete lines, so a concurrent partial
// write is retried on the next poll.
type logCursor struct {
	info   os.FileInfo
	offset int64
}

type logSource struct{ path, component string }

func logSources(root, ticket string) ([]logSource, error) {
	var sources []logSource
	add := func(dir, base, component string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && (entry.Name() == base || strings.HasPrefix(entry.Name(), base+".")) {
				sources = append(sources, logSource{filepath.Join(dir, entry.Name()), component})
			}
		}
		return nil
	}
	if err := add(root, "scheduler.log", "scheduler"); err != nil {
		return nil, err
	}
	repos, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return sources, nil
	}
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if repo.IsDir() {
			if err := add(filepath.Join(root, repo.Name(), ticket), "executor.log", "executor"); err != nil {
				return nil, err
			}
		}
	}
	return sources, nil
}

func streamLogs(ctx context.Context, root, ticket string, cutoff time.Time, follow, asJSON bool, out io.Writer) error {
	var cursors []*logCursor
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		sources, err := logSources(root, ticket)
		if err != nil {
			return err
		}
		var entries []logEntry
		var active []*logCursor
		for _, source := range sources {
			f, err := os.Open(source.path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			if !info.Mode().IsRegular() {
				f.Close()
				continue
			}
			var cursor *logCursor
			for _, existing := range cursors {
				if os.SameFile(info, existing.info) {
					cursor = existing
					break
				}
			}
			if cursor == nil {
				cursor = &logCursor{info: info}
			}
			active = append(active, cursor)
			if info.Size() < cursor.offset {
				cursor.offset = 0
			}
			batch, readErr := readLogLines(f, cursor, source.component, ticket, cutoff, follow)
			closeErr := f.Close()
			if readErr != nil {
				return fmt.Errorf("%s: %w", source.path, readErr)
			}
			if closeErr != nil {
				return closeErr
			}
			entries = append(entries, batch...)
		}
		cursors = active
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
		for _, entry := range entries {
			line := entry.raw
			if !asJSON {
				line = formatLog(entry)
			}
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func readLogLines(f *os.File, cursor *logCursor, component, ticket string, cutoff time.Time, follow bool) ([]logEntry, error) {
	if _, err := f.Seek(cursor.offset, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(f)
	var entries []logEntry
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if err == io.EOF && (follow || len(line) == 0) {
			break
		}
		cursor.offset += int64(len(line))
		raw := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil, fmt.Errorf("invalid JSON log line: %w", err)
		}
		id := logField(fields, "ticket")
		if component == "scheduler" && id != ticket || component == "executor" && id != "" && id != ticket {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, logField(fields, "time"))
		if err != nil {
			return nil, fmt.Errorf("invalid log time: %w", err)
		}
		if !at.Before(cutoff) {
			entries = append(entries, logEntry{raw: raw, fields: fields, at: at, component: component})
		}
	}
	return entries, nil
}

func logField(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func formatLog(entry logEntry) string {
	component := logField(entry.fields, "component")
	if component == "" {
		component = entry.component
	}
	// Quote control characters in headings to keep every record on one line.
	heading := func(s string) string {
		if strings.ContainsAny(s, "\n\r\t\x1b") {
			return fmt.Sprintf("%q", s)
		}
		return s
	}
	parts := []string{entry.at.Format(time.RFC3339Nano), heading(component), heading(logField(entry.fields, "level")), heading(logField(entry.fields, "event"))}
	var keys []string
	for key := range entry.fields {
		if key != "time" && key != "component" && key != "level" && key != "event" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		var compact strings.Builder
		compact.WriteString(heading(key))
		compact.WriteByte('=')
		parts = append(parts, compact.String()+strings.TrimSpace(string(entry.fields[key])))
	}
	return strings.Join(parts, " ")
}
