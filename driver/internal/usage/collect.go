// Package usage reads local agent transcripts without modifying them.
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

type Worktree struct {
	Path string `json:"path"`
	// Role labels Claude sessions only: reviewer or session (the default).
	Role string `json:"role,omitempty"`
}

type Options struct {
	Ticket              string
	Worktrees           []Worktree
	RunWorktree         string
	ClaimedTickets      []string
	RunID               string
	CoordinatorSessions []string
	// Empty directories use the standard agent homes. Explicit directories help
	// callers collect archived transcripts and keep tests independent of $HOME.
	ClaudeDir, CodexDir string
}

type Tokens struct {
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
	Errors       []FileError `json:"errors"`
}

var ticketPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// CollectItems matches normalized session working directories to the ticket's worktrees.
// A corrupt file contributes nothing; other files are still collected. Run
// worktrees must belong to this run and ClaimedTickets counts distinct tickets.
func CollectItems(o Options) (Result, error) {
	result := Result{Ticket: o.Ticket, PriceVersion: prices.Version, Items: []Item{}, Errors: []FileError{}}
	if !ticketPattern.MatchString(o.Ticket) || len(o.Worktrees) == 0 {
		return result, errors.New("usage: valid ticket and associated unit worktrees required")
	}
	roles := map[string]string{}
	for _, w := range o.Worktrees {
		if !filepath.IsAbs(w.Path) || (w.Role != "" && w.Role != "session" && w.Role != "reviewer") {
			return result, errors.New("usage: invalid worktree")
		}
		role := w.Role
		if role == "" {
			role = "session"
		}
		path := normalizePath(w.Path)
		if old, ok := roles[path]; ok && old != role {
			return result, errors.New("usage: conflicting worktree roles")
		}
		roles[path] = role
	}
	if o.RunWorktree != "" {
		if !filepath.IsAbs(o.RunWorktree) || o.RunID == "" || len(o.CoordinatorSessions) == 0 {
			return result, errors.New("usage: coordinator attribution requires run ID, run worktree and coordinator session IDs")
		}
		claims := map[string]bool{}
		for _, ticket := range o.ClaimedTickets {
			if !ticketPattern.MatchString(ticket) {
				return result, errors.New("usage: invalid claimed ticket")
			}
			claims[ticket] = true
		}
		if !claims[o.Ticket] {
			return result, errors.New("usage: current run claimed ticket identifiers missing or omit this ticket")
		}
		o.ClaimedTickets = nil
		for ticket := range claims {
			o.ClaimedTickets = append(o.ClaimedTickets, ticket)
		}
		for _, id := range o.CoordinatorSessions {
			if id == "" {
				return result, errors.New("usage: empty coordinator session ID")
			}
		}
		path := normalizePath(o.RunWorktree)
		if _, ok := roles[path]; ok {
			return result, errors.New("usage: run and unit worktrees overlap")
		}
		roles[path] = "coordinator"
	}
	if o.RunWorktree == "" && (o.RunID != "" || len(o.CoordinatorSessions) > 0 || len(o.ClaimedTickets) > 0) {
		return result, errors.New("usage: coordinator attribution requires run worktree")
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
	for _, id := range o.CoordinatorSessions {
		coordinatorIDs[id] = true
	}
	for _, source := range []struct{ root, provider string }{{o.ClaudeDir, "claude"}, {o.CodexDir, "codex"}} {
		err := filepath.WalkDir(source.root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				result.Errors = append(result.Errors, FileError{path, fmt.Sprintf("cannot read session path: %v", walkErr)})
				return nil
			}
			if !d.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			records, err := readSession(path, source.provider, roles, coordinatorIDs)
			if err != nil {
				result.Errors = append(result.Errors, FileError{path, err.Error()})
				return nil
			}
			if len(records.Items) == 0 && len(records.Messages) == 0 {
				return nil
			}
			items := records.Items
			if source.provider == "claude" {
				// Per API message: a replayed message counts once, subagent files add their own.
				for id, item := range records.Messages {
					if seenMessages[id] {
						continue
					}
					seenMessages[id] = true
					items = append(items, item)
				}
			} else {
				// Codex total_token_usage is cumulative per session: one charge per session.
				key := records.ID
				if seen[key] {
					return nil
				}
				seen[key] = true
			}
			for _, item := range items {
				if item.Role == "coordinator" {
					item.Apportioned = true
					item.Tokens = scale(item.Tokens, 1/float64(len(o.ClaimedTickets)))
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
	return Tokens{a.InputTokens + b.InputTokens, a.OutputTokens + b.OutputTokens, a.CacheReadTokens + b.CacheReadTokens, a.CacheWriteTokens + b.CacheWriteTokens, a.CacheWriteHourTokens + b.CacheWriteHourTokens}
}
func scale(a Tokens, n float64) Tokens {
	return Tokens{a.InputTokens * n, a.OutputTokens * n, a.CacheReadTokens * n, a.CacheWriteTokens * n, a.CacheWriteHourTokens * n}
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
