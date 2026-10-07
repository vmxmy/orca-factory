package scheduler

import (
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/testenv"
	"testing"
	"time"
)

func TestPackageStatusIsolation(t *testing.T) {
	testenv.CheckStatusIsolation(t, func() {

		now := time.Now()
		status.PushObservation(now, func(v *status.View) { v.Tickets = []status.Ticket{{Repo: "/repo", Identifier: "ZII-916"}} })
		if err := status.Write(status.Snapshot{GeneratedAt: now}); err != nil {
			t.Fatal(err)
		}
	})
}
