package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type rawUsage struct {
	Input    float64 `json:"input_tokens"`
	Output   float64 `json:"output_tokens"`
	Read     float64 `json:"cache_read_input_tokens"`
	Write    float64 `json:"cache_creation_input_tokens"`
	Cached    float64 `json:"cached_input_tokens"`
	CodexWrite float64 `json:"cache_write_input_tokens"`
	Creation  struct {
		Hour float64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Reasoning float64 `json:"reasoning_output_tokens"`
}

func (u rawUsage) tokens(codex bool) (Tokens, error) {
	t := Tokens{u.Input, u.Output, u.Read, u.Write - u.Creation.Hour, u.Creation.Hour, u.Reasoning}
	if codex {
		t = Tokens{InputTokens: u.Input - u.Cached, OutputTokens: u.Output, CacheReadTokens: u.Cached, CacheWriteTokens: u.CodexWrite, ReasoningTokens: u.Reasoning}
	}
	if t.InputTokens < 0 || t.OutputTokens < 0 || t.CacheReadTokens < 0 || t.CacheWriteTokens < 0 || t.CacheWriteHourTokens < 0 {
		return Tokens{}, errors.New("invalid token counters")
	}
	return t, nil
}

type record struct {
	Type      string          `json:"type"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"sessionId"`
	Message   json.RawMessage `json:"message"`
	Payload   struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Cwd   string `json:"cwd"`
		Model string `json:"model"`
		Info  *struct {
			Total *rawUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}
type session struct {
	Messages map[string]Item
	ID       string
	Items    []Item
}

// Totals stay private until EOF so a malformed file contributes nothing.
func readSession(path, provider string, roles map[string]string, coordinatorIDs map[string]bool) (result session, err error) {
	f, err := os.Open(path)
	if err != nil {
		return session{}, fmt.Errorf("open session: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			result = session{}
			err = fmt.Errorf("close session: %w", closeErr)
		}
	}()
	reader := bufio.NewReader(f)
	messages := map[string]Item{}
	result = session{}
	cwd, model := "", "unknown"
	var last *Item
	lineNumber := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		lineNumber++
		if len(bytes.TrimSpace(line)) > 0 {
			var r record
			if err := json.Unmarshal(line, &r); err != nil {
				return session{}, fmt.Errorf("line %d: invalid session JSON: %w", lineNumber, err)
			}
			id := r.SessionID
			if provider == "codex" && r.Type == "session_meta" {
				id = r.Payload.ID
				cwd = r.Payload.Cwd
				if r.Payload.Model != "" {
					model = r.Payload.Model
				}
			}
			if id != "" {
				if result.ID != "" && result.ID != id {
					return session{}, errors.New("conflicting session IDs")
				}
				result.ID = id
			}
			if provider == "claude" && r.Cwd != "" {
				cwd = r.Cwd
			}
			if provider == "codex" && r.Type == "turn_context" {
				if r.Payload.Model != "" {
					model = r.Payload.Model
				}
				if r.Payload.Cwd != "" {
					cwd = r.Payload.Cwd
				}
			}
			role, belongs := sessionRole(cwd, roles)
			// A run directory alone cannot identify which of its historical sessions
			// belongs to the requested run. The caller supplies verified session IDs.
			if role == "coordinator" && !coordinatorIDs[result.ID] {
				belongs = false
			}
			if belongs {
				if !filepath.IsAbs(cwd) {
					return session{}, errors.New("session cwd is not absolute")
				}
				if provider == "claude" && r.Type == "assistant" {
					var message struct {
						ID    string    `json:"id"`
						Model string    `json:"model"`
						Usage *rawUsage `json:"usage"`
					}
					if err := json.Unmarshal(r.Message, &message); err != nil {
						return session{}, fmt.Errorf("line %d: invalid assistant message: %w", lineNumber, err)
					}
					if message.Usage != nil {
						if result.ID == "" || message.ID == "" {
							return session{}, errors.New("usage message missing session or message ID")
						}
						tokens, err := message.Usage.tokens(false)
						if err != nil {
							return session{}, fmt.Errorf("line %d: %w", lineNumber, err)
						}
						if message.Model == "" {
							message.Model = "unknown"
						}
						// Streaming/replayed records with the same message ID replace the
						// previous snapshot; only one charge survives for each actual message.
						messages[message.ID] = Item{Role: role, Model: message.Model, Tokens: tokens}
					}
				}
				if provider == "codex" && r.Type == "event_msg" && r.Payload.Type == "token_count" && r.Payload.Info != nil && r.Payload.Info.Total != nil {
					if result.ID == "" {
						return session{}, errors.New("Codex session ID missing")
					}
					tokens, err := r.Payload.Info.Total.tokens(true)
					if err != nil {
						return session{}, fmt.Errorf("line %d: %w", lineNumber, err)
					}
					if role != "coordinator" {
						role = "worker"
					}
					// total_token_usage is cumulative. last_token_usage is a single turn
					// and is deliberately never substituted for the cumulative snapshot.
					last = &Item{Role: role, Model: model, Tokens: tokens}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return session{}, fmt.Errorf("read session at line %d: %w", lineNumber, readErr)
		}
	}
	if provider == "codex" {
		if last != nil {
			result.Items = []Item{*last}
		}
		return result, nil
	}
	result.Messages = messages
	return result, nil
}

func sessionRole(cwd string, roles map[string]string) (string, bool) {
	if !filepath.IsAbs(cwd) {
		return "", false
	}
	cwd = normalizePath(cwd)
	role, best := "", -1
	for root, candidate := range roles {
		rel, err := filepath.Rel(root, cwd)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		// Prefer the closest worktree when nested unit and reviewer directories overlap.
		if len(root) > best {
			role, best = candidate, len(root)
		}
	}
	return role, best >= 0
}
