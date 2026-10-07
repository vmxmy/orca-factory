package status

import (
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/testenv"
)

func TestPackageStatusIsolation(t *testing.T) {
	testenv.CheckStatusIsolation(t, func() {

		now := time.Now()
		PushObservation(now, func(v *View) { v.Tickets = []Ticket{{Repo: "/repo", Identifier: "ZII-916"}} })
		if err := Write(Snapshot{GeneratedAt: now}); err != nil {
			t.Fatal(err)
		}
	})
}
