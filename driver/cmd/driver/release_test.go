package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const releaseSHA = "1234567890abcdef1234567890abcdef12345678"

func TestRelease(t *testing.T) {
	for _, tc := range []struct {
		name, smoke, fail, wantError string
		done                         bool
		want                         []string
	}{
		{name: "success", want: []string{"evidence", "deploy", "doctor", "evidence", "comment"}},
		{name: "done", done: true, want: []string{"evidence", "deploy", "doctor", "evidence", "comment", "done"}},
		{name: "fail", smoke: "fail", done: true, wantError: "credentials: fail", want: []string{"evidence", "deploy", "doctor"}},
		{name: "unknown", smoke: "unknown", wantError: "credentials: unknown", want: []string{"evidence", "deploy", "doctor"}},
		{name: "deploy failure", fail: "deploy", wantError: "deploy", want: []string{"evidence", "deploy"}},
		{name: "doctor failure", fail: "doctor", wantError: "doctor", want: []string{"evidence", "deploy", "doctor"}},
		{name: "comment failure", fail: "comment", done: true, wantError: "comment", want: []string{"evidence", "deploy", "doctor", "evidence", "comment"}},
		{name: "done failure", fail: "done", done: true, wantError: "done", want: []string{"evidence", "deploy", "doctor", "evidence", "comment", "done"}},
		{name: "malformed", smoke: "malformed", wantError: "doctor", want: []string{"evidence", "deploy", "doctor"}},
		{name: "changed evidence", fail: "refresh", wantError: "evidence", want: []string{"evidence", "deploy", "doctor", "evidence"}},
		{name: "empty", smoke: "empty", wantError: "doctor", want: []string{"evidence", "deploy", "doctor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			call := func(name string) error {
				calls = append(calls, name)
				if tc.fail == name {
					return errors.New(name + " unavailable")
				}
				return nil
			}
			deps := releaseDeps{
				paths: func() (string, string, error) { return "/ops/deploy.sh", "/bin/driver", nil },
				evidence: func(_ context.Context, ticket string, pr int, sha string) (merge.Progress, error) {
					if ticket != "OF-461" || pr != 42 || sha != releaseSHA {
						t.Fatalf("wrong inputs: %s %d %s", ticket, pr, sha)
					}
					if tc.fail == "refresh" && len(calls) > 0 {
						return merge.Progress{}, errors.Join(call("evidence"), errors.New("deployment evidence changed"))
					}
					return merge.Progress{Version: 1, QueueID: "frozen-queue", TicketID: "canonical", Repository: "owner/repo", PR: pr, Head: strings.Repeat("a", 40), MergeSHA: sha, Phase: "merged", MainUnknown: true}, call("evidence")
				},
				run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
					if spec.Path == "/ops/deploy.sh" {
						return nil, call("deploy")
					}
					if spec.Path != "/bin/driver" || !reflect.DeepEqual(spec.Args, []string{"doctor", "--json"}) {
						t.Fatalf("unexpected command: %+v", spec)
					}
					err := call("doctor")
					if tc.smoke == "malformed" {
						return []byte("not json"), err
					}
					if tc.smoke == "empty" {
						return []byte(`{"checks":{},"errors":[]}`), err
					}
					status := tc.smoke
					if status == "" {
						status = "ok"
					}
					return []byte(`{"checks":{"version":{"status":"ok","details":{"running":"1234567","factory_main":"` + releaseSHA + `"}},"credentials":{"status":"` + status + `","reason":"probe result"}},"errors":[]}`), err
				},
				comment: func(_ context.Context, id, body string) error {
					if id != "canonical" {
						t.Fatalf("comment id %q", id)
					}
					var progress map[string]any
					line := strings.Split(body, "\n")[0]
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, merge.ProgressPrefix)), &progress); err != nil {
						t.Fatal(err)
					}
					if _, exists := progress["deployment"]; exists {
						t.Fatalf("deployment present: %s", body)
					}
					if progress["queue_id"] != "frozen-queue" || progress["merge_sha"] != releaseSHA || progress["main_unknown"] != true || progress["main"] != "" || progress["pr"] != float64(42) {
						t.Fatalf("wrong receipt: %s", body)
					}
					if !strings.Contains(body, "1234567") || !strings.Contains(body, "doctor --json") {
						t.Fatalf("missing confirmed facts: %s", body)
					}
					return call("comment")
				},
				done: func(_ context.Context, id string) error {
					if id != "canonical" {
						t.Fatal(id)
					}
					return call("done")
				},
			}
			args := []string{"OF-461", "--pr", "42", "--sha", releaseSHA}
			if tc.done {
				args = append(args, "--done")
			}
			var out bytes.Buffer
			code := runRelease(args, &out, deps)
			if (code != 0) != (tc.wantError != "") || !strings.Contains(out.String(), tc.wantError) {
				t.Fatalf("code=%d output=%s", code, &out)
			}
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls=%v want=%v", calls, tc.want)
			}
		})
	}
}

func TestReleaseInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"OF-461"}, {"OF-461", "--pr", "0", "--sha", releaseSHA}, {"OF-461", "--pr", "42", "--sha", "abc"}, {"OF-461", "--pr", "42", "--sha", releaseSHA, "extra"}} {
		var out bytes.Buffer
		if code := runRelease(args, &out, releaseDeps{}); code != 2 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
}

type releaseLedger struct {
	issues.Store
	ticket   issues.Task
	comments []task.Comment
}

func (l releaseLedger) Read(context.Context, string) (issues.Task, error) { return l.ticket, nil }
func (l releaseLedger) ListComments(context.Context, string) ([]task.Comment, error) {
	return l.comments, nil
}

const (
	releaseRecordedQueue = "8b2bf83e0bc3f204cb1ba400e9336875b2ceabfb5b08afda3516c73608129a50"
	releaseRecordedHead  = "1af93d0f2f736fbf02e3f36579f1b8be03f00c2d"
	releaseRecordedMerge = "e51c6389bf0097a6c2a9b500f79c49805b6b1bd1"
)

func releaseRecordedGitHub(t *testing.T) (github.PullRequest, []github.File) {
	t.Helper()
	recorded := map[string]string{
		"/repos/vmxmy/orca-factory/pulls/637": "github-pr-merged-repo.json",
		"/repos/vmxmy/orca-factory/compare/ffbd6bfebe098d1ea0ca9b531de1814ed9940b0c^...ffbd6bfebe098d1ea0ca9b531de1814ed9940b0c": "github-compare-main-files.json",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := recorded[r.URL.Path]
		if !ok {
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := os.ReadFile("../../testdata/recorded/" + name)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := w.Write(raw); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test-secret", nil }})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := client.GetPR(context.Background(), "vmxmy", "orca-factory", 637)
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := client.Compare(context.Background(), "vmxmy", "orca-factory", pr.MergeSHA+"^", pr.MergeSHA)
	if err != nil {
		t.Fatal(err)
	}
	return pr, comparison.Files
}

func releaseRecordedComments(t *testing.T, name string) []task.Comment {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var recording struct{ Comments []task.Comment }
	if err := json.Unmarshal(raw, &recording); err != nil {
		t.Fatal(err)
	}
	return recording.Comments
}

func releaseRewriteLine(t *testing.T, comment task.Comment, prefix string, edit func(map[string]any)) task.Comment {
	t.Helper()
	lines := strings.Split(comment.Body, "\n")
	for i, line := range lines {
		raw, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatal(err)
		}
		edit(fields)
		b, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = prefix + string(b)
		comment.Body = strings.Join(lines, "\n")
		return comment
	}
	t.Fatalf("comment %s has no %s line", comment.ID, prefix)
	return comment
}

// The PR read-back replays github-pr-merged-repo.json (PR 637) and its changed
// files replay github-compare-main-files.json. Comments replay ledger-comments-human-release.json
// (no handoff) and ledger-comments-selfdeploy-interrupted.json (OF-393 handoff
// and progress); budgets are edited to distinct values so the source is visible.
func TestReleaseEvidence(t *testing.T) {
	recordedPR, recordedFiles := releaseRecordedGitHub(t)
	if recordedPR.Number != 637 || recordedPR.MergeSHA != "ffbd6bfebe098d1ea0ca9b531de1814ed9940b0c" || recordedPR.Head.SHA != "30124f99e6a0ef649eba7f4a8b3f72c15d58f4c0" || recordedPR.Base.Repo.FullName != "vmxmy/orca-factory" {
		t.Fatalf("recorded PR=%+v", recordedPR)
	}
	human := releaseRecordedComments(t, "ledger-comments-human-release.json")
	selfDeploy := releaseRecordedComments(t, "ledger-comments-selfdeploy-interrupted.json")
	// The recording redacts the frozen gate and review outputs; restore a
	// validating lane 1 set so only the edited field decides each case.
	handoff := releaseRewriteLine(t, selfDeploy[0], merge.HandoffPrefix, func(fields map[string]any) {
		fields["gate_spec"] = "UNIT: executor\nSCOPE: driver/cmd/driver/release.go\nCHECK: make test"
		fields["gate_output"] = "UNIT executor\nVERDICT green\nSTEP all\nREASON all gates passed\nEVIDENCE\nSCOPE driver/cmd/driver/release.go\nLANE 1\n"
		fields["review_output"] = `{"unit":"executor","verdict":"green","step":"review"}`
	})
	progress := selfDeploy[1]
	withoutDeployment := func(edit func(map[string]any)) task.Comment {
		return releaseRewriteLine(t, progress, merge.ProgressPrefix, func(fields map[string]any) {
			delete(fields, "deployment")
			if edit != nil {
				edit(fields)
			}
		})
	}
	editHandoff := func(edit func(map[string]any)) task.Comment {
		return releaseRewriteLine(t, handoff, merge.HandoffPrefix, edit)
	}
	budget := func(fields map[string]any, attempt, corrections, infra, catchup int) {
		fields["attempt"], fields["corrections"], fields["infra"], fields["catchup_rounds"] = attempt, corrections, infra, catchup
	}
	budgetedHandoff := editHandoff(func(fields map[string]any) { budget(fields, 4, 2, 3, 5) })
	for _, name := range []string{"manual", "handoff", "known main", "unmerged", "wrong sha", "wrong repo", "lane 3", "lane 3 manual", "no files", "invalid head", "invalid budget", "invalid prior budget", "deployment", "held", "wrong head", "unknown phase", "invalid main", "incomplete handoff", "unvalidated handoff"} {
		t.Run(name, func(t *testing.T) {
			ledger := releaseLedger{ticket: issues.Task{Issue: task.Issue{ID: "d8844eec-5be4-4de1-9e4e-c583c8b284a4", Identifier: "OF-393"}}}
			pr := recordedPR
			pr.Number, pr.Head.SHA, pr.MergeSHA = 605, releaseRecordedHead, releaseRecordedMerge
			identifier, number, sha := "OF-393", 605, releaseRecordedMerge
			files := recordedFiles
			switch name {
			case "lane 3 manual":
				files = append(append([]github.File(nil), recordedFiles...), github.File{Filename: ".env.production", Status: "modified"})
				fallthrough
			case "manual":
				ledger = releaseLedger{ticket: issues.Task{Issue: task.Issue{ID: "zy-366", Identifier: "ZY-366"}}, comments: human}
				pr = recordedPR
				identifier, number, sha = "ZY-366", 637, recordedPR.MergeSHA
			case "handoff":
				ledger.comments = []task.Comment{budgetedHandoff}
			case "known main":
				ledger.comments = []task.Comment{budgetedHandoff, withoutDeployment(func(fields map[string]any) { budget(fields, 5, 3, 4, 6) })}
			case "deployment":
				ledger.comments = []task.Comment{handoff, progress}
			case "unknown phase":
				ledger.comments = []task.Comment{handoff, withoutDeployment(func(fields map[string]any) { fields["phase"] = "future-phase" })}
			case "invalid main":
				ledger.comments = []task.Comment{handoff, withoutDeployment(func(fields map[string]any) { fields["main"] = "not-a-sha" })}
			case "unmerged":
				ledger.comments = []task.Comment{handoff}
				pr.Merged = false
			case "wrong sha":
				ledger.comments = []task.Comment{handoff}
				sha = recordedPR.MergeSHA
			case "wrong repo":
				ledger.comments = []task.Comment{handoff}
				pr.Base.Repo.FullName = "other/repo"
			case "lane 3":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { fields["lane"] = 3 })}
			case "wrong head":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { fields["head"] = recordedPR.Head.SHA })}
			case "incomplete handoff":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { delete(fields, "unit") })}
			case "unvalidated handoff":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { fields["review_output"] = "" })}
			case "invalid head":
				pr.Head.SHA = strings.Repeat("z", 40)
			case "invalid budget":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { budget(fields, 0, -1, 0, 0) })}
			case "invalid prior budget":
				ledger.comments = []task.Comment{handoff, withoutDeployment(func(fields map[string]any) { fields["infra"] = -1 })}
			case "no files":
				ledger.comments = []task.Comment{handoff}
				files = nil
			case "held":
				ledger.comments = []task.Comment{handoff}
				ledger.ticket.Metadata.ImportHold = true
			}
			got, err := releaseEvidence(context.Background(), ledger, identifier, number, sha, "vmxmy/orca-factory", strings.Fields(".env* *secrets*"), pr, files)
			if name != "manual" && name != "handoff" && name != "known main" {
				if err == nil {
					t.Fatalf("accepted %s: %+v", name, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != "merged" || got.Deployment != nil || got.Repository != "vmxmy/orca-factory" {
				t.Fatalf("receipt=%+v", got)
			}
			switch name {
			case "manual":
				if got.TicketID != "zy-366" || got.QueueID != "" || got.Main != "" || !got.MainUnknown || got.PR != 637 || got.Head != "30124f99e6a0ef649eba7f4a8b3f72c15d58f4c0" || got.MergeSHA != "ffbd6bfebe098d1ea0ca9b531de1814ed9940b0c" {
					t.Fatalf("manual receipt=%+v", got)
				}
			case "handoff":
				if got.TicketID != "d8844eec-5be4-4de1-9e4e-c583c8b284a4" || got.QueueID != releaseRecordedQueue || got.Attempt != 4 || got.Corrections != 2 || got.Infra != 3 || got.CatchupRounds != 5 || got.Main != "" || !got.MainUnknown || got.PR != 605 || got.Head != releaseRecordedHead || got.MergeSHA != releaseRecordedMerge {
					t.Fatalf("handoff receipt=%+v", got)
				}
			case "known main":
				if got.QueueID != releaseRecordedQueue || got.Main != "a11d72cbc1420ee4123297be52a7f9a378e9a71b" || got.MainUnknown || got.Attempt != 5 || got.Corrections != 3 || got.Infra != 4 || got.CatchupRounds != 6 {
					t.Fatalf("known main receipt=%+v", got)
				}
			}
		})
	}
}

// doctor-json.json is a real `orca-factory-driver doctor --json` read-back
// whose warn checks still count as a passed smoke.
func TestReleaseSmokeRecorded(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/doctor-json.json")
	if err != nil {
		t.Fatal(err)
	}
	running, err := releaseSmoke(raw)
	if err != nil || running != "0aa1ad4027a2f198dfcdfa69aafc04ebbebeb014" {
		t.Fatalf("running=%q err=%v", running, err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	report["checks"].(map[string]any)["ledger"].(map[string]any)["status"] = "unknown"
	failed, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseSmoke(failed); err == nil || !strings.Contains(err.Error(), "ledger: unknown") {
		t.Fatalf("unknown check accepted: %v", err)
	}
}
