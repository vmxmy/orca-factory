package github

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

// recordedServer serves testdata/recorded payloads by request path suffix.
func recordedServer(t *testing.T, routes map[string]string, status map[string]int) *Client {
	t.Helper()
	return fixture(t, func(w http.ResponseWriter, r *http.Request) {
		for suffix, name := range routes {
			if strings.HasSuffix(r.URL.Path, suffix) && (r.Method == http.MethodGet) == (status[suffix] == 0) {
				b, err := os.ReadFile("../../testdata/recorded/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if code := status[suffix]; code != 0 {
					w.WriteHeader(code)
				}
				w.Write(b)
				return
			}
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}, Options{})
}

func TestRecordedMergeLaneReads(t *testing.T) {
	c := recordedServer(t, map[string]string{
		"/pulls/138":                   "github-pr-open-clean.json",
		"/pulls/133":                   "github-pr-merged.json",
		"/commits/main":                "github-commit-main.json",
		"/compare/main...e09d48f":      "github-compare-behind.json",
		"/compare/e09d48f...main":      "github-compare-main-files.json",
		"/commits/c4e86ec5/check-runs": "github-check-runs-gitguardian.json",
	}, nil)
	ctx := context.Background()
	open, err := c.GetPR(ctx, "vmxmy", "orca-factory", 138)
	if err != nil || open.State != "open" || open.Merged || open.MergeableState != "clean" || open.Head.Ref != "probe/merge-lane-recording" || open.Base.Ref != "main" || len(open.Head.SHA) != 40 {
		t.Fatalf("open PR=%+v err=%v", open, err)
	}
	merged, err := c.GetPR(ctx, "vmxmy", "orca-factory", 133)
	if err != nil || !merged.Merged || merged.MergeableState != "unknown" || merged.MergeCommitSHA != "0ab0047178cd32132001eef0682d50a77a6f2622" {
		t.Fatalf("merged PR=%+v err=%v", merged, err)
	}
	if sha, err := c.BranchSHA(ctx, "vmxmy", "orca-factory", "main"); err != nil || sha != merged.MergeCommitSHA {
		t.Fatalf("main sha=%q err=%v", sha, err)
	}
	behind, err := c.Compare(ctx, "vmxmy", "orca-factory", "main", "e09d48f")
	if err != nil || behind.BehindBy != 5 || len(behind.Files) != 0 || len(behind.MergeBaseCommit.SHA) != 40 {
		t.Fatalf("behind compare=%+v err=%v", behind, err)
	}
	mainSide, err := c.Compare(ctx, "vmxmy", "orca-factory", "e09d48f", "main")
	if err != nil || mainSide.BehindBy != 0 || len(mainSide.Files) != 22 || mainSide.Files[0].Filename == "" {
		t.Fatalf("main-side compare=%+v err=%v", mainSide, err)
	}
	runs, err := c.CheckRuns(ctx, "vmxmy", "orca-factory", "c4e86ec5")
	if err != nil || len(runs) != 1 || runs[0].Name != "GitGuardian Security Checks" || runs[0].Status != "completed" || runs[0].Conclusion != "success" || runs[0].HeadSHA != open.Head.SHA {
		t.Fatalf("check runs=%+v err=%v", runs, err)
	}
}

// A squash pinned to a stale head is refused with 409 and nothing merges.
func TestRecordedMergeHeadModified(t *testing.T) {
	c := recordedServer(t, map[string]string{
		"/pulls/138/merge": "github-merge-head-modified-409.json",
		"/pulls/138":       "github-pr-open-clean.json",
	}, map[string]int{"/pulls/138/merge": http.StatusConflict})
	pr, err := c.MergePR(context.Background(), "vmxmy", "orca-factory", 138, strings.Repeat("0", 40))
	var http409 infra.HTTPError
	if pr.Merged || err == nil || !errors.As(err, &http409) || http409.StatusCode != http.StatusConflict {
		t.Fatalf("pr=%+v err=%v", pr, err)
	}
}
