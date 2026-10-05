package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type rawUsage struct {
	Input    float64 `json:"input_tokens"`
	Output   float64 `json:"output_tokens"`
	Read     float64 `json:"cache_read_input_tokens"`
	Write    float64 `json:"cache_creation_input_tokens"`
	Cached   float64 `json:"cached_input_tokens"`
	Creation struct {
		Hour float64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u rawUsage) tokens(codex bool) (Tokens, error) {
	t := Tokens{u.Input, u.Output, u.Read, u.Write - u.Creation.Hour, u.Creation.Hour}
	if codex {
		t.InputTokens = u.Input - u.Cached
		t.CacheReadTokens = u.Cached
	}
	if t.InputTokens < 0 || t.OutputTokens < 0 || t.CacheReadTokens < 0 || t.CacheWriteTokens < 0 || t.CacheWriteHourTokens < 0 {
		return Tokens{}, errors.New("invalid token counters")
	}
	return t, nil
}

type record struct {
	Type    string          `json:"type"`
	Cwd     string          `json:"cwd"`
	Message json.RawMessage `json:"message"`
	Payload struct {
		Type  string `json:"type"`
		Cwd   string `json:"cwd"`
		Model string `json:"model"`
		Info  *struct {
			Total *rawUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// Memory grows with one JSONL record, never with the transcript length. Parsing
// errors deliberately omit transcript contents (which can contain credentials).
func readSession(path, provider string, roles map[string]string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open session")
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	items := map[string]Item{}
	appendItem := func(item Item) {
		key := item.Role + "\x00" + item.Model
		previous := items[key]
		item.Tokens = add(previous.Tokens, item.Tokens)
		items[key] = item
	}
	cwd, model := "", "unknown"
	last := Tokens{}
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var r record
			if json.Unmarshal(line, &r) != nil {
				return nil, errors.New("invalid session JSON")
			}
			if provider == "claude" {
				if r.Cwd != "" {
					cwd = r.Cwd
				}
				role, ok := roles[filepath.Clean(cwd)]
				if ok && r.Type == "assistant" {
					var message struct {
						Model string    `json:"model"`
						Usage *rawUsage `json:"usage"`
					}
					if json.Unmarshal(r.Message, &message) != nil {
						return nil, errors.New("invalid assistant message")
					}
					if message.Usage == nil {
						continue
					}
					t, err := message.Usage.tokens(false)
					if err != nil {
						return nil, err
					}
					m := message.Model
					if m == "" {
						m = "unknown"
					}
					appendItem(Item{Role: role, Model: m, Tokens: t})
				}
			} else {
				if r.Type == "session_meta" {
					cwd = r.Payload.Cwd
				}
				if r.Type == "turn_context" && r.Payload.Model != "" {
					model = r.Payload.Model
				}
				role, ok := roles[filepath.Clean(cwd)]
				if ok && r.Type == "event_msg" && r.Payload.Type == "token_count" && r.Payload.Info != nil && r.Payload.Info.Total != nil {
					current, err := r.Payload.Info.Total.tokens(true)
					if err != nil {
						return nil, err
					}
					// Attribute increments to the active model: the sum equals the final
					// cumulative snapshot, even when a session switches models.
					delta := add(current, scale(last, -1))
					last = current
					if role != "coordinator" {
						role = "worker"
					}
					appendItem(Item{Role: role, Model: model, Tokens: delta})
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, errors.New("cannot read session")
		}
	}
	// Aggregate in-file before returning, retaining bounded memory per model.
	result := make([]Item, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	return result, nil
}
