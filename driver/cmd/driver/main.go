package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/BurntSushi/toml"
	"github.com/vmxmy/orca-factory/driver/internal/logx"
)

var version = "dev"
var commands = make(map[string]func([]string) int)

// Register adds a subcommand to the driver.
func Register(name string, run func(args []string) int) {
	commands[name] = run
}

func init() {
	Register("version", func(args []string) int {
		fmt.Printf("orca-factory-driver %s\n", version)
		return 0
	})
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			fmt.Print(topLevelHelp)
			return 0
		}
		if command, ok := commands[args[0]]; ok {
			return command(args[1:])
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
	}
	fmt.Fprint(os.Stderr, topLevelHelp)
	return 2
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// loggedCommand owns the sink until normal return or panic unwinding completes.
func loggedCommand(component, path string, console io.Writer, args []string, execute func(*slog.Logger) (int, error)) (code int) {
	logger := logx.New(component, console)
	file, openErr := logx.RotatingFile(path, 0, 0)
	if openErr == nil {
		defer func() { _ = file.Close() }() // Descriptor cleanup; log writes are synchronous and unbuffered.
		logger = logx.New(component, io.MultiWriter(file, console))
	}
	started := time.Now()
	var commandErr error
	defer func() {
		if value := recover(); value != nil {
			logger.Error("command panicked", "event", "exit", "exit_code", 2, "err", fmt.Sprint(value), "duration_ms", time.Since(started).Milliseconds())
			panic(value)
		}
		level := slog.LevelInfo
		if code != 0 {
			level = slog.LevelError
		}
		logger.Log(context.Background(), level, "command exited", "event", "exit", "exit_code", code, "err", commandErr, "duration_ms", time.Since(started).Milliseconds())
	}()
	logger.Info("command started", "event", "start", "version", version, "args", redactArgs(args))
	if openErr != nil {
		logger.Error("open log", "event", "log_open_error", "err", openErr)
	}
	code, commandErr = execute(logger)
	if commandErr != nil {
		fmt.Fprintln(os.Stderr, commandErr)
	}
	return code
}

// redactArgs hides credential option values; logx handles token prefixes.
func redactArgs(args []string) []string {
	result := make([]string, len(args))
	hideNext := false
	for i, arg := range args {
		key, _, assigned := strings.Cut(arg, "=")
		if hideNext {
			result[i] = "[REDACTED]"
			hideNext = false
			continue
		}
		private := false
		for _, part := range []string{"authorization", "api_key", "apikey", "token", "password", "secret"} {
			private = private || strings.Contains(strings.ToLower(key), part)
		}
		if strings.HasPrefix(key, "-") && private {
			if assigned {
				result[i] = key + "=[REDACTED]"
			} else {
				result[i] = key
				hideNext = true
			}
		} else {
			result[i] = arg
		}
	}
	return result
}

func logStateRoot() string {
	if root := os.Getenv("AGENT_DRIVER_STATE_ROOT"); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "orca-factory")
}
