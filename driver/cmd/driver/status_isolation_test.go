package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/testsupport"
)

func TestPackageStatusIsolation(t *testing.T) {
	testsupport.CheckStatusIsolation(t, func() {
		TestStatusSnapshotServe(t)
		now := time.Now()
		status.PushObservation(now, now, func(v *status.View) { v.Tickets = []status.Ticket{{Repo: "/repo", Identifier: "ZII-916"}} })
		if err := status.Write(status.Snapshot{GeneratedAt: now}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRecordedStagesPanelSample(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/driver-status-json-stages.json")
	if err != nil {
		t.Fatal(err)
	}
	panelSample, err := os.ReadFile("../../../.claude/skills/ziikoo-driver/test/sample-recorded.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(panelSample, []byte("RECORDED_STAGES_STATUS: string = String.raw`"+string(bytes.TrimSpace(raw))+"`")) {
		t.Fatal("panel RECORDED_STAGES_STATUS differs from driver-status-json-stages.json")
	}
}
