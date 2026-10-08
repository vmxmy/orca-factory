package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func init() { Register("issues", runIssues) }
func issueEnvironment() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"AGENT_ISSUE_BACKEND", "AGENT_GH_REPO", "AGENT_GH_API_URL", "AGENT_LINEAR_TEAM", "AGENT_LINEAR_PROJECT"} {
		env[k] = os.Getenv(k)
	}
	return env
}

type issueLabels []string

func (v *issueLabels) String() string { return strings.Join(*v, ",") }
func (v *issueLabels) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("empty label")
	}
	*v = append(*v, s)
	return nil
}
func issueInput(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("input file required")
	}
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, errors.New("cannot open input")
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if err != nil || len(b) > 32<<20 {
		return nil, errors.New("input too large or unreadable")
	}
	return b, nil
}
func runIssues(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: driver issues snapshot|read|create|adopt|update|comment|export|preview|import|release-hold|validate")
		return 2
	}
	fs := flag.NewFlagSet("issues", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.String("id", "", "canonical ID, identifier or GitHub number")
	input := fs.String("input", "", "JSON file, or - for stdin")
	bodyFile := fs.String("body-file", "", "comment body file, or - for stdin")
	state := fs.String("state", "", "task state")
	apply := fs.Bool("apply", false, "apply validated import")
	_ = fs.Bool("json", true, "machine output")
	var add, remove issueLabels
	fs.Var(&add, "add-label", "label to add")
	fs.Var(&remove, "remove-label", "label to remove")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return 2
	}
	env := issueEnvironment()
	backend, err := issues.Backend(env)
	if args[0] == "preview" {
		raw, inputErr := issueInput(*input)
		var result map[string]any
		if inputErr == nil {
			result, inputErr = issues.Preview(raw, env["AGENT_GH_REPO"])
		}
		if inputErr != nil {
			fmt.Fprintln(os.Stderr, inputErr)
			return 1
		}
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "result": result}) != nil {
			return 1
		}
		return 0
	}
	if err != nil || (backend != "github" && args[0] != "export") {
		fmt.Fprintln(os.Stderr, "issues: AGENT_ISSUE_BACKEND=github required")
		return 2
	}
	ctx, cancel := issueCommandContext(context.Background(), args[0])
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "issues: home directory unavailable")
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "issues: driver configuration could not be loaded")
		return 1
	}
	client, err := issues.New(ctx, env, cfg.Tuning)
	if err != nil {
		fmt.Fprintln(os.Stderr, "issues: backend initialization failed")
		return 1
	}
	if args[0] == "export" {
		source, ok := client.(*linear.Client)
		if !ok {
			fmt.Fprintln(os.Stderr, "export requires explicitly selected authorized Linear source")
			return 2
		}
		manifest, exportErr := issues.Export(ctx, source, env["AGENT_GH_REPO"], env["AGENT_LINEAR_PROJECT"])
		if exportErr != nil {
			fmt.Fprintln(os.Stderr, exportErr)
			return 1
		}
		if json.NewEncoder(os.Stdout).Encode(manifest) != nil {
			return 1
		}
		return 0
	}
	gh := client.(*issues.GitHub)
	var result any
	switch args[0] {
	case "snapshot", "list":
		var rows []issues.Task
		rows, err = gh.List(ctx)
		if err == nil {
			out := []map[string]any{}
			for _, t := range rows {
				var row map[string]any
				row, err = publicIssue(ctx, gh, t, false)
				if err != nil {
					break
				}
				out = append(out, row)
			}
			result = map[string]any{"issues": out, "complete": true}
		}
	case "read":
		var t issues.Task
		t, err = gh.Read(ctx, *id)
		if err == nil {
			var row map[string]any
			row, err = publicIssue(ctx, gh, t, true)
			result = map[string]any{"issue": row}
		}
	case "release-hold":
		if !*apply {
			err = errors.New("release-hold requires explicit --apply")
			break
		}
		var raw []byte
		raw, err = issueInput(*input)
		if err == nil {
			result, err = issues.ReleaseHold(ctx, gh, raw)
		}
	case "adopt":
		var raw []byte
		raw, err = issueInput(*input)
		if err == nil {
			result, err = gh.Adopt(ctx, *id, raw)
		}
	case "create":
		var raw []byte
		raw, err = issueInput(*input)
		if err == nil {
			var in issues.CreateInput
			err = issues.DecodeInput(raw, &in)
			if err == nil {
				var t issues.Task
				t, err = gh.Create(ctx, in)
				if err == nil {
					var row map[string]any
					row, err = publicIssue(ctx, gh, t, true)
					result = map[string]any{"issue": row}
				}
			}
		}
	case "update":
		if *input != "" {
			var raw []byte
			raw, err = issueInput(*input)
			if err == nil {
				_, err = gh.Edit(ctx, *id, raw)
			}
		}
		if err == nil && (*state != "" || len(add) > 0 || len(remove) > 0) {
			_, err = gh.UpdateIssue(ctx, *id, *state, add, remove)
		}
		if err == nil {
			var t issues.Task
			t, err = gh.Read(ctx, *id)
			if err == nil {
				var row map[string]any
				row, err = publicIssue(ctx, gh, t, true)
				result = map[string]any{"issue": row}
			}
		}
	case "comment":
		var raw []byte
		raw, err = issueInput(*bodyFile)
		if err == nil {
			c, e := gh.CreateComment(ctx, *id, string(raw))
			err = e
			result = map[string]any{"comment": c}
		}
	case "import":
		var raw []byte
		raw, err = issueInput(*input)
		if err == nil {
			result, err = issues.Import(ctx, gh, raw, *apply)
		}
	case "validate":
		_, err = gh.List(ctx)
		result = map[string]any{"repository": gh.Repository(), "backend": "github", "complete": true}
	default:
		fmt.Fprintln(os.Stderr, "issues: unknown command")
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "issues: %v\n", err)
		return 1
	}
	if json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "result": result}) != nil {
		return 1
	}
	return 0
}
func publicIssue(ctx context.Context, c *issues.GitHub, t issues.Task, withComments bool) (map[string]any, error) {
	var ownership scheduler.Ticket
	if withComments {
		comments, err := c.ListComments(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.Comments.Nodes = comments
		ownership, err = scheduler.InspectOwnership(comments)
		if err != nil {
			return nil, err
		}
	} else {
		t.Comments.PageInfo.HasNextPage = true
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	result["state"] = map[string]string{"id": t.State.ID, "name": t.State.Name, "type": t.State.ID}
	result["commentsComplete"] = withComments
	if withComments {
		result["owner"] = ownership.Owner
	}
	result["phase"] = ownership.Phase
	result["parent"] = t.Metadata.Parent
	return result, nil
}

func issueCommandContext(parent context.Context, command string) (context.Context, context.CancelFunc) {
	timeout := 10 * time.Minute
	if command == "import" {
		timeout = 30 * time.Minute
	}
	return context.WithTimeout(parent, timeout)
}
