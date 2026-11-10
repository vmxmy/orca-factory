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

func releaseRecordedPR(t *testing.T) github.PullRequest {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/recorded/github-pr-merged.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/vmxmy/orca-factory/pulls/133" {
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
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
	pr, err := client.GetPR(context.Background(), "vmxmy", "orca-factory", 133)
	if err != nil {
		t.Fatal(err)
	}
	return pr
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

// The PR read-back replays github-pr-merged.json (PR 133). Comments replay
// ledger-comments-human-release.json (no handoff) and
// ledger-comments-selfdeploy-interrupted.json (OF-393 handoff and progress).
func TestReleaseEvidence(t *testing.T) {
	recordedPR := releaseRecordedPR(t)
	if recordedPR.Number != 133 || recordedPR.MergeSHA != "0ab0047178cd32132001eef0682d50a77a6f2622" || recordedPR.Head.SHA != "e9efe2887b6bc803d7c2a71c381fbc45cd3cc40a" {
		t.Fatalf("recorded PR=%+v", recordedPR)
	}
	human := releaseRecordedComments(t, "ledger-comments-human-release.json")
	selfDeploy := releaseRecordedComments(t, "ledger-comments-selfdeploy-interrupted.json")
	handoff, progress := selfDeploy[0], selfDeploy[1]
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
	for _, name := range []string{"manual", "handoff", "known main", "unmerged", "wrong sha", "lane 3", "deployment", "held", "wrong head", "unknown phase", "invalid main"} {
		t.Run(name, func(t *testing.T) {
			ledger := releaseLedger{ticket: issues.Task{Issue: task.Issue{ID: "d8844eec-5be4-4de1-9e4e-c583c8b284a4", Identifier: "OF-393"}}}
			pr := recordedPR
			pr.Number, pr.Head.SHA, pr.MergeSHA = 605, releaseRecordedHead, releaseRecordedMerge
			identifier, number, sha := "OF-393", 605, releaseRecordedMerge
			switch name {
			case "manual":
				ledger = releaseLedger{ticket: issues.Task{Issue: task.Issue{ID: "zy-366", Identifier: "ZY-366"}}, comments: human}
				pr = recordedPR
				identifier, number, sha = "ZY-366", 133, recordedPR.MergeSHA
			case "handoff":
				ledger.comments = []task.Comment{handoff}
			case "known main":
				ledger.comments = []task.Comment{handoff, withoutDeployment(nil)}
			case "deployment":
				ledger.comments = selfDeploy
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
			case "lane 3":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { fields["lane"] = 3 })}
			case "wrong head":
				ledger.comments = []task.Comment{editHandoff(func(fields map[string]any) { fields["head"] = recordedPR.Head.SHA })}
			case "held":
				ledger.comments = []task.Comment{handoff}
				ledger.ticket.Metadata.ImportHold = true
			}
			got, err := releaseEvidence(context.Background(), ledger, identifier, number, sha, "vmxmy/orca-factory", pr)
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
				if got.TicketID != "zy-366" || got.QueueID != "" || got.Main != "" || !got.MainUnknown || got.PR != 133 || got.Head != "e9efe2887b6bc803d7c2a71c381fbc45cd3cc40a" || got.MergeSHA != "0ab0047178cd32132001eef0682d50a77a6f2622" {
					t.Fatalf("manual receipt=%+v", got)
				}
			case "handoff":
				if got.TicketID != "d8844eec-5be4-4de1-9e4e-c583c8b284a4" || got.QueueID != releaseRecordedQueue || got.Attempt != 1 || got.Corrections != 0 || got.Main != "" || !got.MainUnknown || got.PR != 605 || got.Head != releaseRecordedHead || got.MergeSHA != releaseRecordedMerge {
					t.Fatalf("handoff receipt=%+v", got)
				}
			case "known main":
				if got.QueueID != releaseRecordedQueue || got.Main != "a11d72cbc1420ee4123297be52a7f9a378e9a71b" || got.MainUnknown || got.Attempt != 1 {
					t.Fatalf("known main receipt=%+v", got)
				}
			}
		})
	}
}
