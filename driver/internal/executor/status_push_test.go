package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func TestStatusFilePush(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "transition updates"
		if fail {
			name = "write failure does not stop execution"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if fail {
				dir = filepath.Join(dir, "blocked")
				if err := os.WriteFile(dir, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDG_RUNTIME_DIR", dir)
			e, _, _, _, _, ticket := fixture(t, "new-sha")
			result, err := e.Execute(context.Background(), ticket)
			if err != nil || result.State.Owner != machine.OwnerMerge {
				t.Fatalf("progress: %+v %v", result, err)
			}
			if fail {
				return
			}
			data, err := os.ReadFile(filepath.Join(dir, "orca-factory", "status.json"))
			if err != nil {
				t.Fatal(err)
			}
			var view status.View
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatal(err)
			}
			if view.State != "ok" || !view.GeneratedAt.Equal(e.Now()) || len(view.Tickets) != 1 || view.Tickets[0].Phase != string(machine.PhaseMergeQueue) || view.Tickets[0].Owner != string(machine.OwnerMerge) {
				t.Fatalf("view: %+v", view)
			}
		})
	}
}
