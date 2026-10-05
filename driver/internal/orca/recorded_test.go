package orca

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

func TestRecordedRequestMismatch(t *testing.T) {
	fixture, err := filepath.Abs("../../testdata/recorded/orca-run-create-request-mismatch.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{Path: script}
	_, err = c.RunCreate(context.Background(), "6e2f5bc0-7a6f-53e0-bff8-b8cdf5d2e070", "objective")
	var ticket *infra.Ticket
	if !errors.As(err, &ticket) || !strings.Contains(err.Error()+ticket.Err.Error(), "request_mismatch") {
		t.Fatalf("err=%v, want a non-retryable error naming request_mismatch", err)
	}
}
