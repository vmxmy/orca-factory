package main

// logs reads only ticket-scoped local evidence: launch stderr at
// $HOME/.local/state/orca-factory/launch-<ticket>.log and FAIL history at
// <state-root>/*/<ticket>/fail.log. The state root defaults to
// $HOME/.local/state/orca-factory (AGENT_DRIVER_STATE_ROOT may override it).
// Both sources are free text, one record per line; timestamps at the start
// (RFC3339/RFC3339Nano or common date + time formats) are parsed best-effort.
// JSON object lines are also accepted and kept verbatim in raw. Lines without
// a recognizable timestamp have zero time, sort first, and survive --since.
// No Linear request or shared scheduler/executor log path is required.

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
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

func init() { Register("logs", runLogs) }

func runLogs(args []string) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	follow := flags.Bool("follow", false, "follow new records")
	since := flags.String("since", "", "show records from the last duration")
	asJSON := flags.Bool("json", false, "write a structured JSON array (raw preserves each line)")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: driver logs <票号> [--follow] [--since 时长] [--json]")
	}
	var positional []string
	// FlagSet stops at the first positional argument; parse again after it
	// so flags work on either side of the ticket.
	for len(args) > 0 {
		if args[0] == "--" {
			positional = append(positional, args[1:]...)
			break
		}
		if err := flags.Parse(args); err != nil {
			return 2
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}
	if len(positional) != 1 || !logComponent.MatchString(positional[0]) || positional[0] == "." || positional[0] == ".." {
		flags.Usage()
		return 2
	}
	var cutoff time.Time
	providedSince := false
	flags.Visit(func(f *flag.Flag) { providedSince = providedSince || f.Name == "since" })
	if providedSince {
		duration, err := time.ParseDuration(*since)
		if err != nil || duration < 0 {
			fmt.Fprintf(os.Stderr, "invalid --since duration: %q\n", *since)
			flags.Usage()
			return 2
		}
		cutoff = time.Now().Add(-duration)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := streamLogs(ctx, os.Stdout, logStateRoot(), positional[0], cutoff, *asJSON, *follow); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type logsRecord struct {
	Time      time.Time      `json:"time"`
	Source    string         `json:"source"`
	Component string         `json:"component"`
	Level     string         `json:"level"`
	Event     string         `json:"event"`
	Fields    map[string]any `json:"fields"`
	Raw       string         `json:"raw"`
}

type logsFile struct {
	path      string
	component string
	file      *os.File
	offset    int64
	info      os.FileInfo
}

func logsLiveFiles(root, ticket string) ([]*logsFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", ticket, "fail.log"))
	if err != nil {
		return nil, err
	}
	files := []*logsFile{{path: filepath.Join(home, ".local", "state", "orca-factory", "launch-"+ticket+".log"), component: "launch"}}
	for _, path := range paths {
		files = append(files, &logsFile{path: path, component: "executor"})
	}
	return files, nil
}

func parseLogsRecord(line string, state *logsFile) logsRecord {
	record := logsRecord{Source: state.path, Component: state.component, Level: "-", Event: "-", Fields: map[string]any{}, Raw: line}
	text := strings.TrimSpace(line)
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) == nil && object != nil {
		for key, value := range object {
			switch key {
			case "time":
				if value, ok := value.(string); ok {
					record.Time, _ = logsTimestamp(value)
				}
			case "component", "level", "event":
				if value, ok := value.(string); ok && value != "" {
					switch key {
					case "component":
						record.Component = value
					case "level":
						record.Level = value
					case "event":
						record.Event = value
					}
				}
			default:
				record.Fields[key] = value
			}
		}
		return record
	}
	record.Time, text = logsTimestamp(text)
	parts := strings.Fields(text)
	if len(parts) > 0 {
		level := strings.Trim(parts[0], "[]:")
		switch level {
		case "DEBUG", "INFO", "WARN", "WARNING", "ERROR", "FATAL", "TRACE":
			record.Level = level
			parts = parts[1:]
		case "FAIL":
			record.Level, record.Event = "ERROR", "FAIL"
			parts = parts[1:]
		}
	}
	for _, part := range parts {
		if key, value, ok := strings.Cut(part, "="); ok && key != "" {
			record.Fields[key] = value
		}
	}
	// Unstructured messages remain visible in the fifth column and raw.
	record.Fields["message"] = strings.TrimSpace(text)
	return record
}

func logsTimestamp(text string) (time.Time, string) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006/01/02 15:04:05"} {
		// RFC3339 has one token; the other formats have a date and time.
		parts := strings.Fields(text)
		tokens := 1
		if strings.Contains(layout, " ") {
			tokens = 2
		}
		if len(parts) < tokens {
			continue
		}
		candidate := strings.Join(parts[:tokens], " ")
		if when, err := time.Parse(layout, candidate); err == nil {
			end := strings.Index(text, parts[tokens-1]) + len(parts[tokens-1])
			return when, strings.TrimSpace(text[end:])
		}
	}
	return time.Time{}, text
}

// readLogsFile retains the opened inode and its consumed offset. A replaced
// path first drains the old descriptor; a truncated file restarts at zero.
func readLogsFile(state *logsFile, cutoff time.Time, follow bool) (records []logsRecord, err error) {
	info, statErr := os.Stat(state.path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if state.file != nil && (statErr != nil || !os.SameFile(state.info, info)) {
		batch, err := readOpenLogsFile(state, cutoff, follow)
		if err != nil {
			return nil, err
		}
		records = append(records, batch...)
		if err := state.file.Close(); err != nil {
			return nil, err
		}
		state.file, state.info, state.offset = nil, nil, 0
	}
	if statErr != nil {
		return records, nil
	}
	if state.file == nil {
		state.file, err = os.Open(state.path)
		if errors.Is(err, os.ErrNotExist) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
	}
	batch, err := readOpenLogsFile(state, cutoff, follow)
	return append(records, batch...), err
}

func readOpenLogsFile(state *logsFile, cutoff time.Time, follow bool) ([]logsRecord, error) {
	info, err := state.file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < state.offset {
		state.offset = 0
	}
	if state.info != nil && info.Size() == state.offset && info.ModTime().Equal(state.info.ModTime()) {
		return nil, nil
	}
	state.info = info
	reader := bufio.NewReader(io.NewSectionReader(state.file, state.offset, info.Size()-state.offset))
	records := []logsRecord{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read %s: %w", state.path, err)
		}
		if errors.Is(err, io.EOF) && (len(line) == 0 || follow) {
			return records, nil
		}
		state.offset += int64(len(line))
		record := parseLogsRecord(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), state)
		if cutoff.IsZero() || record.Time.IsZero() || !record.Time.Before(cutoff) {
			records = append(records, record)
		}
		if errors.Is(err, io.EOF) {
			return records, nil
		}
	}
}

func writeLogs(out io.Writer, records []logsRecord, asJSON bool) error {
	sort.SliceStable(records, func(i, j int) bool { return records[i].Time.Before(records[j].Time) })
	if asJSON {
		if records == nil {
			records = []logsRecord{}
		}
		return json.NewEncoder(out).Encode(records)
	}
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	clean := strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`)
	for _, record := range records {
		timestamp := "-"
		if !record.Time.IsZero() {
			timestamp = record.Time.Format(time.RFC3339Nano)
		}
		fields := "-"
		if len(record.Fields) > 0 {
			data, err := json.Marshal(record.Fields)
			if err != nil {
				return err
			}
			fields = string(data)
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", timestamp, clean.Replace(record.Component), clean.Replace(record.Level), clean.Replace(record.Event), fields); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func readLogsBatch(root, ticket string, cutoff time.Time, files []*logsFile, follow bool) ([]logsRecord, []*logsFile, error) {
	live, err := logsLiveFiles(root, ticket)
	if err != nil {
		return nil, files, err
	}
	known := make(map[string]bool, len(files))
	for _, file := range files {
		known[file.path] = true
	}
	for _, file := range live {
		if !known[file.path] {
			files = append(files, file)
		}
	}
	var records []logsRecord
	for _, file := range files {
		batch, err := readLogsFile(file, cutoff, follow)
		if err != nil {
			return nil, files, err
		}
		records = append(records, batch...)
	}
	return records, files, nil
}

// --json --follow emits an array per nonempty polling batch. History is one
// array; human output remains one line per record in either mode.
func streamLogs(ctx context.Context, out io.Writer, root, ticket string, cutoff time.Time, asJSON, follow bool) (err error) {
	var files []*logsFile
	defer func() {
		for _, file := range files {
			if file.file != nil {
				err = errors.Join(err, file.file.Close())
			}
		}
	}()
	history, next, err := readLogsBatch(root, ticket, cutoff, files, follow)
	files = next
	if err != nil {
		return err
	}
	if err := writeLogs(out, history, asJSON); err != nil || !follow {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			batch, next, err := readLogsBatch(root, ticket, cutoff, files, follow)
			files = next
			if err != nil {
				return err
			}
			if len(batch) > 0 {
				if err := writeLogs(out, batch, asJSON); err != nil {
					return err
				}
			}
		}
	}
}
