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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func init() { Register("logs", runLogs) }

func runLogs(args []string) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	follow := flags.Bool("follow", false, "follow new records")
	since := flags.String("since", "", "show records from the last duration")
	asJSON := flags.Bool("json", false, "write original JSON lines")
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
	raw    string
	when   time.Time
	fields map[string]json.RawMessage
}

type logsFile struct {
	path   string
	shared bool
	offset int64
	info   os.FileInfo
}

func logsLiveFiles(root, ticket string) ([]*logsFile, error) {
	dirs, err := filepath.Glob(filepath.Join(root, "*", ticket))
	if err != nil {
		return nil, err
	}
	files := []*logsFile{
		{path: filepath.Join(root, "scheduler.log"), shared: true},
		{path: filepath.Join(root, "launch-"+ticket+".log")},
	}
	// launchLog redirects stderr under HOME even when the executor's state
	// root is overridden. Also discover launch files in the selected root.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	launch := filepath.Join(home, ".local", "state", "orca-factory", "launch-"+ticket+".log")
	if filepath.Clean(launch) != filepath.Clean(files[1].path) {
		files = append(files, &logsFile{path: launch})
	}
	for _, dir := range dirs {
		files = append(files, &logsFile{path: filepath.Join(dir, "executor.log")})
	}
	return files, nil
}

func logsRotations(path string) ([]string, error) {
	paths, err := filepath.Glob(path + ".*")
	if err != nil {
		return nil, err
	}
	type rotation struct {
		path   string
		number int
	}
	var rotations []rotation
	for _, name := range paths {
		number, err := strconv.Atoi(strings.TrimPrefix(name, path+"."))
		if err == nil && number > 0 {
			rotations = append(rotations, rotation{name, number})
		}
	}
	sort.SliceStable(rotations, func(i, j int) bool { return rotations[i].number > rotations[j].number })
	paths = nil
	for _, rotation := range rotations {
		paths = append(paths, rotation.path)
	}
	return paths, nil
}

func readLogsFile(state *logsFile, ticket string, cutoff time.Time) (records []logsRecord, err error) {
	file, err := os.Open(state.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if state.info == nil || !os.SameFile(state.info, info) || info.Size() < state.offset {
		state.offset = 0
	}
	state.info = info
	reader := bufio.NewReader(io.NewSectionReader(file, state.offset, info.Size()-state.offset))
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			// Leave an unfinished line at the offset for the next poll.
			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", state.path, err)
		}
		state.offset += int64(len(line))
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &fields) != nil {
			continue
		}
		// Scoped files may omit ticket metadata (command start/exit records),
		// but explicit metadata must never name a different ticket. Shared
		// scheduler files require an exact structured ticket identifier.
		hasTicket, matches := false, true
		for _, key := range []string{"ticket", "identifier"} {
			if value, ok := fields[key]; ok {
				hasTicket = true
				matches = matches && logsString(value) == ticket
			}
		}
		if !matches || (state.shared && !hasTicket) {
			continue
		}
		when, err := time.Parse(time.RFC3339Nano, logsString(fields["time"]))
		if err != nil || (!cutoff.IsZero() && !when.After(cutoff)) {
			continue
		}
		records = append(records, logsRecord{raw: line, when: when, fields: fields})
	}
}

func logsString(value json.RawMessage) string {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return ""
}

func writeLogs(out io.Writer, records []logsRecord, asJSON bool) error {
	sort.SliceStable(records, func(i, j int) bool { return records[i].when.Before(records[j].when) })
	for _, record := range records {
		line := record.raw
		if !asJSON {
			fields := record.fields
			event := logsString(fields["event"])
			if event == "" {
				event = logsString(fields["msg"])
			}
			parts := []string{logsString(fields["time"]), "[" + logsString(fields["component"]) + "]", logsString(fields["level"]), event}
			var keys []string
			for key := range fields {
				if key != "time" && key != "component" && key != "level" && key != "event" && (key != "msg" || logsString(fields["event"]) != "") {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				value := string(fields[key])
				if len(value) > 0 && value[0] == '"' {
					value = logsString(fields[key])
				}
				parts = append(parts, key+"="+value)
			}
			line = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(strings.Join(parts, " ")) + "\n"
		}
		if _, err := io.WriteString(out, line); err != nil {
			return err
		}
	}
	return nil
}

// readLogsBatch tracks offsets by file identity so a rename during rotation
// cannot discard unread records from the previous live file.
func readLogsBatch(root, ticket string, cutoff time.Time, previous []*logsFile) ([]logsRecord, []*logsFile, error) {
	live, err := logsLiveFiles(root, ticket)
	if err != nil {
		return nil, nil, err
	}
	var records []logsRecord
	var current []*logsFile
	for _, file := range live {
		rotations, err := logsRotations(file.path)
		if err != nil {
			return nil, nil, err
		}
		for _, path := range append(rotations, file.path) {
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			state := &logsFile{path: path, shared: file.shared}
			for _, old := range previous {
				if old.info != nil && os.SameFile(old.info, info) {
					state.offset, state.info = old.offset, old.info
					break
				}
			}
			batch, err := readLogsFile(state, ticket, cutoff)
			if err != nil {
				return nil, nil, err
			}
			records = append(records, batch...)
			current = append(current, state)
		}
	}
	return records, current, nil
}

// streamLogs prints history and polls live and rotated files until canceled.
func streamLogs(ctx context.Context, out io.Writer, root, ticket string, cutoff time.Time, asJSON, follow bool) error {
	history, files, err := readLogsBatch(root, ticket, cutoff, nil)
	if err != nil {
		return err
	}
	if err := writeLogs(out, history, asJSON); err != nil || !follow {
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			batch, next, err := readLogsBatch(root, ticket, cutoff, files)
			if err != nil {
				return err
			}
			files = next
			if err := writeLogs(out, batch, asJSON); err != nil {
				return err
			}
		}
	}
}
