package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

type Worktree struct {
	Path string `json:"path"`
}

type Options struct {
	Ticket             string
	Worktrees          []Worktree
	ReviewerWorktree   string
	RunWorktree        string
	CoordinatorSession string
	ClaimedTicketCount int
	// Explicit roots allow archived sessions to be collected independently of the host.
	ClaudeDir, CodexDir string
}

type Tokens struct {
	// ReasoningTokens is a subset of output, never an additional charge.
	ReasoningTokens float64 `json:"reasoning_tokens"`
	// InputTokens excludes cache reads/writes for both providers.
	InputTokens          float64 `json:"input_tokens"`
	OutputTokens         float64 `json:"output_tokens"`
	CacheReadTokens      float64 `json:"cache_read_tokens"`
	CacheWriteTokens     float64 `json:"cache_write_tokens"`
	CacheWriteHourTokens float64 `json:"cache_write_hour_tokens"`
}

type Item struct {
	Role  string `json:"role"`
	Model string `json:"model"`
	Tokens
	CostUSD      float64 `json:"cost_usd"`
	UnknownModel bool    `json:"unknown_model"`
	Apportioned  bool    `json:"apportioned"`
}

type FileError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}
type Result struct {
	Ticket       string      `json:"ticket"`
	PriceVersion string      `json:"price_version"`
	Items        []Item      `json:"items"`
	Skipped      []FileError `json:"skipped"`
}

var ticketPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

func collectItems(o Options) (Result, error) {
	result := Result{Ticket: o.Ticket, PriceVersion: prices.Version, Items: []Item{}, Skipped: []FileError{}}
	if !ticketPattern.MatchString(o.Ticket) {
		return result, errors.New("usage: valid ticket required")
	}
	roles := map[string]string{}
	for _, w := range o.Worktrees {
		if !filepath.IsAbs(w.Path) {
			return result, errors.New("usage: worktree must be absolute")
		}
		roles[normalizePath(w.Path)] = "session"
	}
	if o.ReviewerWorktree != "" {
		if !filepath.IsAbs(o.ReviewerWorktree) {
			return result, errors.New("usage: reviewer worktree must be absolute")
		}
		roles[normalizePath(o.ReviewerWorktree)] = "reviewer"
	}
	if o.RunWorktree != "" || o.CoordinatorSession != "" || o.ClaimedTicketCount != 0 {
		if !filepath.IsAbs(o.RunWorktree) || o.CoordinatorSession == "" || o.ClaimedTicketCount < 1 {
			return result, errors.New("usage: coordinator requires absolute run worktree, session ID and positive claimed ticket count")
		}
		path := normalizePath(o.RunWorktree)
		if _, ok := roles[path]; ok {
			return result, errors.New("usage: run and unit worktrees overlap")
		}
		roles[path] = "coordinator"
	}
	if len(roles) == 0 {
		return result, nil
	}
	if o.ClaudeDir == "" || o.CodexDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return result, err
		}
		if o.ClaudeDir == "" {
			root := os.Getenv("CLAUDE_CONFIG_DIR")
			if root == "" {
				root = filepath.Join(home, ".claude")
			}
			o.ClaudeDir = filepath.Join(root, "projects")
		}
		if o.CodexDir == "" {
			root := os.Getenv("CODEX_HOME")
			if root == "" {
				root = filepath.Join(home, ".codex")
			}
			o.CodexDir = filepath.Join(root, "sessions")
		}
	}
	totals := map[string]Item{}
	seen := map[string]bool{}
	seenMessages := map[string]bool{}
	coordinatorIDs := map[string]bool{}
	if o.CoordinatorSession != "" {
		coordinatorIDs[o.CoordinatorSession] = true
	}
	for _, source := range []struct{ root, provider string }{{o.ClaudeDir, "claude"}, {o.CodexDir, "codex"}} {
		err := filepath.WalkDir(source.root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				result.Skipped = append(result.Skipped, FileError{path, fmt.Sprintf("cannot read session path: %v", walkErr)})
				return nil
			}
			if !d.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			records, err := readSession(path, source.provider, roles, coordinatorIDs)
			if err != nil {
				result.Skipped = append(result.Skipped, FileError{path, err.Error()})
				return nil
			}
			if len(records.Items) == 0 && len(records.Messages) == 0 {
				return nil
			}
			if source.provider == "claude" {
				for id, item := range records.Messages {
					key := records.ID + "\x00" + id
					if seenMessages[key] {
						continue
					}
					seenMessages[key] = true
					records.Items = append(records.Items, item)
				}
			} else {
				if seen[records.ID] {
					return nil
				}
				seen[records.ID] = true
			}
			for _, item := range records.Items {
				if item.Role == "coordinator" {
					item.Apportioned = true
					item.Tokens = scale(item.Tokens, 1/float64(o.ClaimedTicketCount))
				}
				key := fmt.Sprintf("%s\x00%s\x00%t", item.Role, item.Model, item.Apportioned)
				prev := totals[key]
				prev.Role = item.Role
				prev.Model = item.Model
				prev.Apportioned = item.Apportioned
				prev.Tokens = add(prev.Tokens, item.Tokens)
				totals[key] = prev
			}
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	keys := make([]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		item := totals[key]
		item.CostUSD, item.UnknownModel = cost(item.Model, item.Tokens)
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func add(a, b Tokens) Tokens {
	return Tokens{a.ReasoningTokens + b.ReasoningTokens, a.InputTokens + b.InputTokens, a.OutputTokens + b.OutputTokens, a.CacheReadTokens + b.CacheReadTokens, a.CacheWriteTokens + b.CacheWriteTokens, a.CacheWriteHourTokens + b.CacheWriteHourTokens}
}
func scale(a Tokens, n float64) Tokens {
	return Tokens{a.ReasoningTokens * n, a.InputTokens * n, a.OutputTokens * n, a.CacheReadTokens * n, a.CacheWriteTokens * n, a.CacheWriteHourTokens * n}
}

// normalizePath resolves symlink aliases when the directory is available.
func normalizePath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// Embedded JSON is validated during initialization.
func decodePrices(data []byte) priceTable {
	var p priceTable
	if err := json.Unmarshal(data, &p); err != nil {
		panic(err)
	}
	return p
}

// Optional attribution retains the existing ledger contract while allowing callers
// to identify shared reviewer and coordinator sessions explicitly.
func Collect(ctx context.Context, ticket string, worktrees []string, attribution ...Options) (TicketUsage, error) {
	if err := ctx.Err(); err != nil {
		return TicketUsage{}, err
	}
	if len(attribution) > 1 {
		return TicketUsage{}, errors.New("usage: at most one attribution option is allowed")
	}
	options := Options{}
	if len(attribution) == 1 {
		options = attribution[0]
	}
	options.Ticket = ticket
	options.Worktrees = nil
	for _, path := range worktrees {
		options.Worktrees = append(options.Worktrees, Worktree{Path: path})
	}
	result, err := collectItems(options)
	if err != nil {
		return TicketUsage{}, err
	}
	out := TicketUsage{Items: result.Items, Skipped: result.Skipped, Ticket: result.Ticket, PriceVersion: result.PriceVersion, ByModel: map[string]Totals{}}
	for _, item := range result.Items {
		part := Totals{
			Reasoning:  int64(math.Round(item.ReasoningTokens)),
			Input:      int64(math.Round(item.InputTokens)),
			Output:     int64(math.Round(item.OutputTokens)),
			CacheRead:  int64(math.Round(item.CacheReadTokens)),
			CacheWrite: int64(math.Round(item.CacheWriteTokens + item.CacheWriteHourTokens)),
			CostUSD:    item.CostUSD,
		}
		out.Totals = out.add(part)
		out.ByModel[item.Model] = out.ByModel[item.Model].add(part)
		switch item.Role {
		case "worker":
			out.ByRole.Worker = out.ByRole.Worker.add(part)
		case "reviewer":
			out.ByRole.Reviewer = out.ByRole.Reviewer.add(part)
		case "coordinator":
			out.ByRole.Coordinator = out.ByRole.Coordinator.add(part)
		default:
			out.ByRole.Session = out.ByRole.Session.add(part)
		}
		out.Apportioned = out.Apportioned || item.Apportioned
	}
	return out, nil
}

func (t Totals) add(o Totals) Totals {
	return Totals{t.Input + o.Input, t.Output + o.Output, t.CacheRead + o.CacheRead, t.CacheWrite + o.CacheWrite, t.Reasoning + o.Reasoning, t.CostUSD + o.CostUSD}
}
