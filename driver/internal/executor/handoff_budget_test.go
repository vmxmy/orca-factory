package executor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

type handoffTaskReadRetry struct {
	Orca
	failed bool
}

func (o *handoffTaskReadRetry) TaskList(ctx context.Context, run string) ([]orca.Task, error) {
	if !o.failed {
		o.failed = true
		return nil, errors.New("temporary task read failure")
	}
	return o.Orca.TaskList(ctx, run)
}

func TestStartHandoffRestoresHistoricalInfraOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		checkpoint  int
		readFailure string
		want        int
	}{
		{"same history", 1, "", 1},
		{"older checkpoint", 0, "", 1},
		{"newer checkpoint", 2, "", 2},
		{"no checkpoint", -1, "", 1},
		{"new failure before checkpoint read", 1, "head", 2},
		{"new failure after checkpoint read", 1, "task", 2},
		{"older checkpoint plus new failure", 0, "task", 2},
		{"no checkpoint plus new failure", -1, "head", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, o, tools, s, ticket := lostChildFixture(t)
			handoff, err := e.Start(context.Background(), ticket)
			if err != nil || handoff.State.Infra != 1 {
				t.Fatalf("handoff fixture: %+v %v", handoff.State, err)
			}
			g := e.GitHub.(*fakeGitHub)
			g.existing = &handoff.PR
			g.existing.Head.SHA = tools.head
			e.Config.Merging = true
			if tc.checkpoint < 0 {
				if err := os.Remove(checkpointPath(e.Config, ticket)); err != nil {
					t.Fatal(err)
				}
			} else {
				cp, err := readCheckpoint(e.Config, ticket)
				if err != nil {
					t.Fatal(err)
				}
				cp.Infra = tc.checkpoint
				if err := saveCheckpoint(e.Config, ticket, *cp); err != nil {
					t.Fatal(err)
				}
			}
			if tc.readFailure == "head" {
				e.Tools = &mergeHeadRetry{recoveryTools: tools}
			}
			if tc.readFailure == "task" {
				e.Orca = &handoffTaskReadRetry{Orca: o}
			}
			result, err := e.Start(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Infra != tc.want || result.State.Corrections != 1 || result.State.Attempt != 2 || len(o.starts) != 2 || s.children != 1 {
				t.Fatalf("history/current reads conflated: state=%+v want infra=%d err=%v starts=%d children=%d", result.State, tc.want, err, len(o.starts), s.children)
			}
		})
	}
}
