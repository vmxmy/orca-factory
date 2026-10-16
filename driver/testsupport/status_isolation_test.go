package testsupport

import (
	"os"
	"strings"
	"testing"
)

func TestIsolateStatusKeepsStateRootInTemp(t *testing.T) {
	t.Setenv("AGENT_DRIVER_STATE_ROOT", "/must/not/be/used")
	t.Setenv("AGENT_ISSUE_BACKEND", "feishu")
	cleanup := IsolateStatus()
	defer cleanup()
	root := os.Getenv("AGENT_DRIVER_STATE_ROOT")
	if root == "" || !strings.HasPrefix(root, os.Getenv("TMPDIR")) {
		t.Fatalf("AGENT_DRIVER_STATE_ROOT=%q, want a path under the isolated temp root", root)
	}
	if os.Getenv("AGENT_ISSUE_BACKEND") != "" {
		t.Fatal("production AGENT_ISSUE_BACKEND leaked into the test")
	}
}
