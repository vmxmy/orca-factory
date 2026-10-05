package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() { Register("logs", runLogs) }

type logEntry struct {
	Time      time.Time
	Component string
	Level     string
	Event     string
	Message   string
	Ticket    string
	Raw       map[string]any
}

// Malformed records do not prevent reading the rest of a log file.
func parseLogLine(line []byte) (logEntry, bool) {
	if !json.Valid(line) {
		return logEntry{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return logEntry{}, false
	}
	field := func(key string) string { value, _ := raw[key].(string); return value }
	at, err := time.Parse(time.RFC3339Nano, field("time"))
	if err != nil {
		return logEntry{}, false
	}
	ticket := field("ticket")
	if ticket == "" {
		ticket = field("ticket_id")
	}
	if ticket == "" {
		ticket = field("issue")
	}
	return logEntry{Time: at, Component: field("component"), Level: field("level"), Event: field("event"), Message: field("msg"), Ticket: ticket, Raw: raw}, true
}

// Return directory errors to the caller; only missing files mean empty history.
func logPaths(stateRoot, repo, ticket string) ([]string, error) {
	dir := filepath.Join(stateRoot, repo, ticket)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	type rotation struct {
		path   string
		number uint64
	}
	var rotations []rotation
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), "executor.log.")
		if !ok || suffix == "" {
			continue
		}
		digits := true
		for _, char := range suffix {
			if char < '0' || char > '9' {
				digits = false
				break
			}
		}
		if !digits {
			continue
		}
		number, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil || number == 0 {
			continue
		}
		rotations = append(rotations, rotation{filepath.Join(dir, entry.Name()), number})
	}
	sort.SliceStable(rotations, func(i, j int) bool { return rotations[i].number > rotations[j].number })
	paths := []string{filepath.Join(stateRoot, "scheduler.log")}
	for _, rotation := range rotations {
		paths = append(paths, rotation.path)
	}
	return append(paths, filepath.Join(dir, "executor.log")), nil
}

func collectLogs(stateRoot, repo, ticket string) ([]logEntry, error) {
	paths, err := logPaths(stateRoot, repo, ticket)
	if err != nil {
		return nil, err
	}
	var entries []logEntry
	for i, path := range paths {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			entry, ok := parseLogLine(scanner.Bytes())
			if !ok || (i == 0 && entry.Ticket != ticket) {
				continue
			}
			if entry.Component == "" {
				if i == 0 {
					entry.Component = "scheduler"
				} else {
					entry.Component = "executor"
				}
			}
			entries = append(entries, entry)
		}
		readErr := scanner.Err()
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%s: %w", path, readErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.Before(entries[j].Time) })
	return entries, nil
}

func formatHuman(e logEntry) string {
	heading := func(s string) string {
		if strings.ContainsAny(s, "\n\r\t\x1b ") {
			return strconv.Quote(s)
		}
		return s
	}
	parts := []string{e.Time.Format(time.RFC3339), heading(e.Component), heading(e.Level), heading(e.Event)}
	var keys []string
	for key := range e.Raw {
		if key != "time" && key != "component" && key != "level" && key != "event" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, heading(key)+"="+encodeLogValue(e.Raw[key]))
	}
	return strings.Join(parts, " ")
}

func formatJSON(e logEntry) string { return encodeLogValue(e.Raw) }

func encodeLogValue(value any) string {
	// Values come exclusively from valid JSON decoded with UseNumber.
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("invalid parsed log entry: %v", err))
	}
	return string(data)
}

func runLogs(args []string) int { return writeLogs(os.Stdout, args) }

func writeLogs(w io.Writer, args []string) int {
	usage := func(reason string) int {
		if reason != "" {
			fmt.Fprintln(os.Stderr, "logs:", reason)
		}
		fmt.Fprintln(os.Stderr, "Usage: driver logs <ticket> [--repo owner/name] [--follow] [--since duration] [--json]")
		return 2
	}
	var ticket, repo string
	var follow, asJSON, hasSince bool
	var since time.Duration
	// Accept flags before or after the ticket; the standard flag package stops
	// at the first positional argument.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--follow":
			follow = true
		case "--json":
			asJSON = true
		case "--repo", "--since":
			flag := args[i]
			i++
			if i == len(args) {
				return usage("missing value for " + flag)
			}
			if flag == "--repo" {
				repo = args[i]
			} else {
				var err error
				since, err = time.ParseDuration(args[i])
				if err != nil || since < 0 {
					return usage("invalid --since duration: " + args[i])
				}
				hasSince = true
			}
		default:
			if strings.HasPrefix(args[i], "-") || ticket != "" {
				return usage("unexpected argument: " + args[i])
			}
			ticket = args[i]
		}
	}
	if ticket == "" || ticket == "." || ticket == ".." || strings.ContainsAny(ticket, "/\\") {
		return usage("a ticket identifier is required")
	}
	if repo == "" {
		repo = os.Getenv("AGENT_GH_REPO")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || strings.Contains(repo, "\\") {
		return usage("set AGENT_GH_REPO or pass --repo owner/name")
	}
	root := os.Getenv("AGENT_DRIVER_STATE_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "logs:", err)
			return 1
		}
		root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	var cutoff, last time.Time
	if hasSince {
		cutoff = time.Now().Add(-since)
	}
	var interrupted chan os.Signal
	if follow {
		interrupted = make(chan os.Signal, 1)
		signal.Notify(interrupted, os.Interrupt)
		defer signal.Stop(interrupted)
	}
	first := true
	var ticker *time.Ticker
	if follow {
		ticker = time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
	}
	for {
		entries, err := collectLogs(root, repo, ticket)
		if err != nil {
			fmt.Fprintln(os.Stderr, "logs:", err)
			return 1
		}
		previous := last
		for _, entry := range entries {
			if hasSince && !entry.Time.After(cutoff) {
				continue
			}
			if !first && !entry.Time.After(previous) {
				continue
			}
			line := formatHuman(entry)
			if asJSON {
				line = formatJSON(entry)
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				fmt.Fprintln(os.Stderr, "logs:", err)
				return 1
			}
			last = entry.Time
		}
		if !follow {
			return 0
		}
		first = false
		select {
		case <-interrupted:
			return 0
		case <-ticker.C:
		}
	}
}
