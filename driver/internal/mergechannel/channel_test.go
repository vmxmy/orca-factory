package mergechannel

import (
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

func TestHandoffBranch(t *testing.T) {
	h := merge.Handoff{Identifier: "OF-466", Unit: "executor"}
	if got := HandoffBranch(&h); got != "agent/OF-466/executor" {
		t.Fatalf("branch = %q, want agent/OF-466/executor", got)
	}
}
