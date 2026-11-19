package main

import (
	"bytes"
	"os"
	"testing"
)

func TestRecordedResolverTopPanelSample(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/driver-config-json-resolver-top.json")
	if err != nil {
		t.Fatal(err)
	}
	panelSample, err := os.ReadFile("../../../.claude/skills/ziikoo-driver/test/sample-recorded.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(panelSample, []byte("RECORDED_RESOLVER_TOP_CONFIG: string = String.raw`"+string(bytes.TrimSpace(raw))+"`")) {
		t.Fatal("panel RECORDED_RESOLVER_TOP_CONFIG differs from driver-config-json-resolver-top.json")
	}
}
