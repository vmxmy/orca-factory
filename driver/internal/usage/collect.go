// Package usage reads local agent transcripts without modifying them.
package usage

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type Worktree struct {
	Path string `json:"path"`
	// Role labels Claude sessions only: reviewer or session (the default).
	Role string `json:"role,omitempty"`
}

type Options struct {
	Ticket         string
	Worktrees      []Worktree
	RunWorktree    string
	ClaimedTickets int
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

// Collect matches exact session working directories to the ticket's worktrees.
// A corrupt file contributes nothing; other files are still collected. Run
// worktrees must belong to this run and ClaimedTickets counts distinct tickets.
func Collect(o Options) (Result, error) {
	result := Result{Ticket: o.Ticket, PriceVersion: prices.Version, Items: []Item{}, Errors: []FileError{}}
	roles := map[string]string{}
	for _, w := range o.Worktrees {
		if !filepath.IsAbs(w.Path) || (w.Role != "" && w.Role != "session" && w.Role != "reviewer") {
			return result, errors.New("usage: invalid worktree")
		}
		role := w.Role
		if role == "" {
			role = "session"
		}
		path := filepath.Clean(w.Path)
		if old, ok := roles[path]; ok && old != role {
			return result, errors.New("usage: conflicting worktree roles")
		}
		roles[path] = role
	}
	if o.RunWorktree != "" {
		if !filepath.IsAbs(o.RunWorktree) || o.ClaimedTickets < 1 {
			return result, errors.New("usage: run worktree requires positive claimed ticket count")
		}
		path := filepath.Clean(o.RunWorktree)
		if _, ok := roles[path]; ok {
			return result, errors.New("usage: run and unit worktrees overlap")
		}
		roles[path] = "coordinator"
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
	for _, source := range []struct{ root, provider string }{{o.ClaudeDir, "claude"}, {o.CodexDir, "codex"}} {
		err := filepath.WalkDir(source.root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				result.Errors = append(result.Errors, FileError{path, "cannot read session path"})
				return nil
			}
			if !d.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			records, err := readSession(path, source.provider, roles)
			if err != nil {
				result.Errors = append(result.Errors, FileError{path, err.Error()})
				return nil
			}
			for _, item := range records {
				if item.Role == "coordinator" {
					item.Apportioned = true
					item.Tokens = scale(item.Tokens, 1/float64(o.ClaimedTickets))
				}
				key := item.Role + "\x00" + item.Model
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

// Embedded JSON is validated during initialization; a bad table is a build error.
func decodePrices(data []byte) priceTable {
	var p priceTable
	if err := json.Unmarshal(data, &p); err != nil {
		panic(err)
	}
	return p
}
