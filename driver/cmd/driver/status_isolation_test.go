package main

import (
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/testsupport"
	"testing"
	"time"
)

func TestPackageStatusIsolation(t *testing.T) {
	testsupport.CheckStatusIsolation(t, func() {
		TestStatusSnapshotServe(t)
		now := time.Now()
		status.PushObservation(now, func(v *status.View) { v.Tickets = []status.Ticket{{Repo: "/repo", Identifier: "ZII-916"}} })
		if err := status.Write(status.Snapshot{GeneratedAt: now}); err != nil {
			t.Fatal(err)
		}
	})
}
