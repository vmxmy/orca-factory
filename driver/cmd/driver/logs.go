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
	"strconv"
	"strings"
	"syscall"
	"time"
)

func init() { Register("logs", runLogs) }

type logEntry struct {
	Time      time.Time      `json:"time"`
	Component string         `json:"component"`
	Level     string         `json:"level"`
	Event     string         `json:"event"`
	Fields    map[string]any `json:"fields"`
	Raw       string         `json:"raw"`
	original  json.RawMessage
}

// Existing JSON records retain every original field and number representation.
func (e logEntry) MarshalJSON() ([]byte, error) {
	if e.original != nil {
		return e.original, nil
	}
	type plainEntry logEntry
	return json.Marshal(plainEntry(e))
}

func runLogs(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runLogsWithContext(ctx, args, logStateRoot(), time.Now(), os.Stdout, os.Stderr)
}

func runLogsWithContext(ctx context.Context, args []string, root string, now time.Time, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("follow", false, "follow new log records until interrupted")
	since := fs.Duration("since", 0, "show records newer than this duration (0 means all)")
	asJSON := fs.Bool("json", false, "output original JSON records")
	fs.Usage = func() { fmt.Fprintln(stderr, "Usage: driver logs <ticket> [--follow] [--since duration] [--json]") }
	// flag stops at the first positional argument. Move the leading ticket so
	// the documented ticket-first invocation also uses standard flag parsing.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fs.Usage()
		return 2
	}
	if fs.NArg() != 1 || !logComponent.MatchString(fs.Arg(0)) || *since < 0 {
		fs.Usage()
		return 2
	}
	ticket := fs.Arg(0)
	// Match the identifier rather than a prefix such as ZII-9010; filter the
	// raw line before decoding any structured data.
	match := regexp.MustCompile(`(?i)(^|[^a-z0-9_-])` + regexp.QuoteMeta(ticket) + `([^a-z0-9_-]|$)`)
	var cutoff time.Time
	if *since > 0 {
		cutoff = now.Add(-*since)
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	var cursors []*logCursor
	var ticker *time.Ticker
	if *follow {
		ticker = time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
	}
	for {
		if ctx.Err() != nil {
			return 0
		}
		paths, err := logPaths(root, ticket)
		if err != nil {
			fmt.Fprintln(stderr, "logs:", err)
			return 1
		}
		entries := make([]logEntry, 0)
		var next []*logCursor
		for _, path := range paths {
			rows, cursor, err := readLogRows(path, cursors, *follow, match, cutoff)
			if err != nil {
				fmt.Fprintln(stderr, "logs:", err)
				return 1
			}
			if cursor != nil {
				next = append(next, cursor)
			}
			entries = append(entries, rows...)
		}
		cursors = next
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.Before(entries[j].Time) })
		if *asJSON && !*follow {
			err = encoder.Encode(entries)
		} else {
			for _, entry := range entries {
				if *asJSON {
					err = encoder.Encode(entry)
				} else {
					_, err = fmt.Fprintln(out, formatHuman(entry))
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "logs:", err)
			return 1
		}
		if !*follow {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// serve writes the shared scheduler.log at the state root; ticket-local
// scheduler.log is also accepted. Repository discovery needs no Git or config.
func logPaths(root, ticket string) ([]string, error) {
	paths := []string{filepath.Join(root, "scheduler.log")}
	repositories, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	for _, repository := range repositories {
		if !repository.IsDir() {
			continue
		}
		dir := filepath.Join(root, repository.Name(), ticket)
		files, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			name := file.Name()
			if name != "scheduler.log" && name != "executor.log" {
				suffix, ok := strings.CutPrefix(name, "executor.log.")
				if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
					continue
				}
			}
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths, nil
}

type logCursor struct {
	info   os.FileInfo
	offset int64
}

// Track file identity as well as offset so renaming executor.log to .1 does
// not replay it, and a new executor.log starts at byte zero.
func readLogRows(path string, cursors []*logCursor, follow bool, match *regexp.Regexp, cutoff time.Time) ([]logEntry, *logCursor, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }() // Read-only descriptor cleanup.
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	cursor := &logCursor{info: info}
	for _, previous := range cursors {
		if os.SameFile(info, previous.info) {
			cursor.offset = previous.offset
			break
		}
	}
	if info.Size() < cursor.offset {
		cursor.offset = 0
	}
	if _, err := file.Seek(cursor.offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	// A bounded snapshot prevents an active writer from starving cancellation.
	reader := bufio.NewReader(io.LimitReader(file, info.Size()-cursor.offset))
	var entries []logEntry
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		if errors.Is(err, io.EOF) && (follow || line == "") {
			break
		}
		cursor.offset += int64(len(line))
		line = strings.TrimRight(line, "\r\n")
		if match.MatchString(line) {
			component := "executor"
			if filepath.Base(path) == "scheduler.log" {
				component = "scheduler"
			}
			if entry, ok := parseLogLine(line, component); ok && (cutoff.IsZero() || entry.Time.After(cutoff)) {
				entries = append(entries, entry)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return entries, cursor, nil
}

var logField = regexp.MustCompile(`([a-zA-Z0-9_.-]+)=("(?:\\.|[^"\\])*"|[^\s]+)`)

func parseLogLine(line, component string) (logEntry, bool) {
	entry := logEntry{Raw: line, Component: component, Fields: make(map[string]any)}
	var timestamp string
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &fields) != nil {
			return entry, false
		}
		entry.original = json.RawMessage(line)
		for key, value := range fields {
			var text string
			_ = json.Unmarshal(value, &text) // Non-string values stay raw in Fields.
			switch key {
			case "time":
				timestamp = text
			case "component":
				if text != "" {
					entry.Component = text
				}
			case "level":
				entry.Level = text
			case "event":
				entry.Event = text
			default:
				entry.Fields[key] = value
			}
		}
	} else {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			return entry, false
		}
		timestamp = parts[0]
		for _, pair := range logField.FindAllStringSubmatch(line, -1) {
			key, value := pair[1], pair[2]
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			switch key {
			case "component":
				entry.Component = value
			case "level":
				entry.Level = value
			case "event":
				entry.Event = value
			default:
				entry.Fields[key] = value
			}
		}
	}
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	entry.Time = at
	return entry, err == nil
}

func formatHuman(entry logEntry) string {
	quote := func(value string) string {
		if strings.ContainsAny(value, " \t\r\n\x1b") {
			return strconv.Quote(value)
		}
		return value
	}
	parts := []string{entry.Time.Format(time.RFC3339Nano), "component=" + quote(entry.Component), "level=" + quote(entry.Level), "event=" + quote(entry.Event)}
	keys := make([]string, 0, len(entry.Fields))
	for key := range entry.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var value string
		switch v := entry.Fields[key].(type) {
		case string:
			value = quote(v)
		case json.RawMessage:
			var text string
			if json.Unmarshal(v, &text) == nil {
				value = quote(text)
			} else {
				value = string(v)
			}
		}
		parts = append(parts, quote(key)+"="+value)
	}
	return strings.Join(parts, "  ")
}
