package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Contract tests read payloads recorded from the real systems (testdata/recorded).
// Hand-written fakes encode our assumptions; recorded payloads encode the systems.
func recorded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordedOwnershipUnregistered(t *testing.T) {
	var resp struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []linear.Comment } `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorded(t, "linear-comment-ownership-unregistered.json"), &resp); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, resp.Data.Issue.Comments.Nodes); err != nil {
		t.Fatal(err)
	}
	if !unregistered(ticket) {
		t.Fatalf("scheduler claim with term=- must read as unregistered, got terminal %q", ticket.Terminal)
	}
}

func TestRecordedTerminalList(t *testing.T) {
	raw := recorded(t, "orca-terminal-list.json")
	r := &Runtime{OrcaPath: "/usr/bin/orca", Run: func(context.Context, runx.Spec) ([]byte, error) { return raw, nil }}
	if absent, err := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_640d9460-b9fd-46a0-bedb-09763c8c20ea"}); err != nil || absent {
		t.Fatalf("listed terminal: absent=%v err=%v", absent, err)
	}
	if absent, err := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_gone"}); err != nil || !absent {
		t.Fatalf("missing terminal: absent=%v err=%v", absent, err)
	}
}
