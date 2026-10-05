package infra

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func gitFailure(t *testing.T, stderrFile string) error {
	t.Helper()
	path := "../../testdata/recorded/" + stderrFile
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	_, err := exec.Command("sh", "-c", `cat "$1" >&2; exit 128`, "git", path).Output()
	if err == nil {
		t.Fatal("expected failure")
	}
	return err
}

func TestRecordedGitNetworkFailureIsTransient(t *testing.T) {
	var transient *Transient
	if err := Classify(gitFailure(t, "git-stderr-tls-failure.txt")); !errors.As(err, &transient) {
		t.Fatalf("TLS handshake failure must be transient, got %T", err)
	}
	if err := Classify(gitFailure(t, "git-stderr-auth-failure.txt")); errors.As(err, &transient) {
		t.Fatal("an HTTP 403 is not a network blip and must not be retried")
	}
}
