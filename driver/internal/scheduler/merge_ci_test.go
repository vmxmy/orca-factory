package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// ciSnapshotGitHub feeds a real github.Client's MergeChecks result into a
// merge.Snapshot, so the merge channel's decision runs on the same parsing
// code the scheduler uses, against recorded check-runs payloads.
type ciSnapshotGitHub struct {
	client      *github.Client
	owner, repo string
	head, main  string
}

func (g *ciSnapshotGitHub) Snapshot(ctx context.Context, _ merge.Request) (merge.Snapshot, error) {
	checks, err := g.client.MergeChecks(ctx, g.owner, g.repo, g.head)
	if err != nil {
		return merge.Snapshot{}, err
	}
	return merge.Snapshot{Head: g.head, Main: g.main, CIHead: g.head, CIGreen: checks.Green, CIFailed: checks.Failed, FailedChecks: checks.FailedNames, MergeableState: "clean", Behind: false}, nil
}
func (g *ciSnapshotGitHub) Squash(context.Context, merge.Request, string, string) (string, error) {
	return "", errors.New("squash must not run for a non-ready snapshot")
}

func checkRunsFixture(t *testing.T, body []byte) *github.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if _, err := w.Write(body); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/status"):
			if _, err := w.Write([]byte(`{"total_count":0,"statuses":[]}`)); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test-secret", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestMergeCIFailureCorrection replays real PR head check-runs payloads: a
// finished run with failures (PR #423's recording) returns Correction naming
// every failed check, a finished green run keeps the existing merge path, and
// an unfinished run still waits.
func TestMergeCIFailureCorrection(t *testing.T) {
	t.Run("finished with failures returns correction", func(t *testing.T) {
		head := "14e794e30d716ef29d1c095de0db3e47b21c534c"
		body, err := os.ReadFile("../../testdata/recorded/github-check-runs-failure.json")
		if err != nil {
			t.Fatal(err)
		}
		gh := &ciSnapshotGitHub{client: checkRunsFixture(t, body), owner: "vmxmy", repo: "orca-factory", head: head, main: "main"}
		c := merge.New(gh, nil, nil, nil)
		if err := c.Enqueue(merge.Request{Repository: "vmxmy/orca-factory", Number: 421}); err != nil {
			t.Fatal(err)
		}
		r, err := c.Next(context.Background(), "vmxmy/orca-factory")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != merge.Correction || r.Corrections != 1 {
			t.Fatalf("want correction, got %+v", r)
		}
		for _, name := range []string{"Driver lint", "Driver vet", "Driver gate", "Driver build", "Driver vuln"} {
			if !strings.Contains(r.Reason, name) {
				t.Fatalf("reason %q missing failed check %q", r.Reason, name)
			}
		}
	})
	t.Run("finished green keeps the existing merge path", func(t *testing.T) {
		head := "c4e86ec5465d1e2d1f6aea45053e663b54a1350b"
		body, err := os.ReadFile("../../testdata/recorded/github-check-runs-gitguardian.json")
		if err != nil {
			t.Fatal(err)
		}
		gh := &ciSnapshotGitHub{client: checkRunsFixture(t, body), owner: "vmxmy", repo: "orca-factory", head: head, main: "main"}
		c := merge.New(gh, nil, nil, nil)
		if err := c.Enqueue(merge.Request{Repository: "vmxmy/orca-factory", Number: 138}); err != nil {
			t.Fatal(err)
		}
		r, err := c.Next(context.Background(), "vmxmy/orca-factory")
		if err == nil || err.Error() != "squash merge unconfirmed" {
			t.Fatalf("want green CI to reach the existing squash path, got result=%+v err=%v", r, err)
		}
	})
	t.Run("unfinished CI still waits", func(t *testing.T) {
		head := strings.Repeat("f", 40)
		body := []byte(fmt.Sprintf(`{"total_count":1,"check_runs":[{"id":1,"name":"ci","status":"in_progress","head_sha":%q}]}`, head))
		gh := &ciSnapshotGitHub{client: checkRunsFixture(t, body), owner: "vmxmy", repo: "orca-factory", head: head, main: "main"}
		c := merge.New(gh, nil, nil, nil)
		if err := c.Enqueue(merge.Request{Repository: "vmxmy/orca-factory", Number: 1}); err != nil {
			t.Fatal(err)
		}
		r, err := c.Next(context.Background(), "vmxmy/orca-factory")
		if err != nil || r.Status != merge.Waiting || r.Corrections != 0 {
			t.Fatalf("want waiting, got result=%+v err=%v", r, err)
		}
	})
}
