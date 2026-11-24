package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

func TestVerifyFreshLaunchIgnoresProgressUpdate(t *testing.T) {
	const ownership = "阶段: worker 运行中\n归属: 执行器 term=- run=- attempt=1"
	const handoff = merge.HandoffPrefix + `{"version":1,"queue_id":"q1","head":"head-a"}`
	const resolution = resolver.Prefix + `{"version":1,"attempt":1,"phase":"waiting"}`
	for _, tc := range []struct {
		name       string
		handoff    string
		resolution string
		want       error
	}{
		{name: "progress only", handoff: handoff, resolution: resolution},
		{name: "handoff changed", handoff: merge.HandoffPrefix + `{"version":1,"queue_id":"q1","head":"head-b"}`, resolution: resolution, want: errLaunchEvidenceChanged},
		{name: "resolution changed", handoff: handoff, resolution: resolver.Prefix + `{"version":1,"attempt":2,"phase":"waiting"}`, want: errLaunchEvidenceChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimed := evidenceComments(ownership, handoff, resolution,
				merge.ProgressPrefix+`{"version":1,"queue_id":"q1","phase":"waiting","infra":1}`)
			ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "ENG-1", Repo: "/repo"}, claimed)
			fresh := evidenceComments(ownership, tc.handoff, tc.resolution,
				merge.ProgressPrefix+`{"version":1,"queue_id":"q1","phase":"waiting","infra":2}`)
			client := &fakeIssues{issueComments: fresh}
			runtime := &Runtime{Clients: map[string]issues.Store{ticket.Repo: client}}
			if err := runtime.verifyFreshLaunch(context.Background(), ticket); !errors.Is(err, tc.want) {
				t.Fatalf("verifyFreshLaunch() = %v, want %v", err, tc.want)
			}
		})
	}
}
