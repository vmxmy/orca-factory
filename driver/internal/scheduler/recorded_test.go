package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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

func TestRecordedExecutorHandoff(t *testing.T) {
	var resp struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []linear.Comment } `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorded(t, "linear-comments-executor-handoff.json"), &resp); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, resp.Data.Issue.Comments.Nodes); err != nil {
		t.Fatal(err)
	}
	if ticket.Owner != machine.OwnerHuman {
		t.Fatalf("a 转人 comment hands the ticket to a human, got owner %q", ticket.Owner)
	}
}

func TestRecordedSchedulerBlockerIdentifiers(t *testing.T) {
	var response struct{ Data struct{ Issue linear.Issue } }
	if err := json.Unmarshal(recorded(t, "linear-inverse-relations.json"), &response); err != nil {
		t.Fatal(err)
	}
	issue := response.Data.Issue
	issue.Project.ID = "project"
	issue.State.Name = "Todo"
	issue.Labels.Nodes = []linear.NamedID{{Name: "ready-for-agent"}}
	client := &fakeLinear{issues: []linear.Issue{issue}}
	repo := config.Repository{Path: "repo", Env: map[string]string{"AGENT_LINEAR_PROJECT": "project", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Reader: client, Clients: map[string]LinearClient{"repo": client}}
	tickets, err := runtime.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || !tickets[0].Blocked || tickets[0].BlockersIncomplete || !reflect.DeepEqual(tickets[0].Blockers, []string{"ZII-893"}) {
		t.Fatalf("lost recorded blocker identifiers: %+v", tickets)
	}
}
