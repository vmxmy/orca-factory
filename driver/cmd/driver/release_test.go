package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestReleaseEvidence(t *testing.T) {
	for _, name := range []string{"manual", "handoff", "known main", "unmerged", "wrong sha", "wrong repo", "lane 3", "deployment", "held", "wrong head", "unknown phase", "invalid main"} {
		t.Run(name, func(t *testing.T) {
			ledger := releaseLedger{ticket: issues.Task{Issue: task.Issue{ID: "canonical", Identifier: "OF-461"}}}
			pr := github.PullRequest{Number: 42, Merged: true, MergeSHA: releaseSHA}
			pr.Head.SHA = strings.Repeat("a", 40)
			pr.Base.Ref, pr.Base.Repo.FullName = "main", "owner/repo"
			h := merge.Handoff{Version: 1, TicketID: "canonical", Identifier: "OF-461", Repository: "owner/repo", PR: 42, Head: pr.Head.SHA, Lane: 2, QueueID: "frozen", Corrections: 2, Infra: 3, Attempt: 4, CatchupRounds: 5}
			p := merge.Progress{Version: 1, TicketID: "canonical", Repository: "owner/repo", PR: 42, Head: pr.Head.SHA, QueueID: "frozen", Main: strings.Repeat("b", 40), Phase: "merge-intent", Corrections: 3, Infra: 4, Attempt: 5, CatchupRounds: 6}
			switch name {
			case "unknown phase":
				p.Phase = "future-phase"
			case "invalid main":
				p.Main = "not-a-sha"
			case "unmerged":
				pr.Merged = false
			case "wrong sha":
				pr.MergeSHA = strings.Repeat("c", 40)
			case "wrong repo":
				pr.Base.Repo.FullName = "other/repo"
			case "lane 3":
				h.Lane = 3
			case "deployment":
				p.Deployment = &merge.DeploymentProgress{State: "pending"}
			case "held":
				ledger.ticket.Metadata.ImportHold = true
			case "wrong head":
				h.Head = strings.Repeat("d", 40)
			}
			if name != "manual" {
				b, err := json.Marshal(h)
				if err != nil {
					t.Fatal(err)
				}
				ledger.comments = append(ledger.comments, task.Comment{ID: "1", Body: merge.HandoffPrefix + string(b)})
			}
			if name == "known main" || name == "deployment" || name == "unknown phase" || name == "invalid main" {
				b, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				ledger.comments = append(ledger.comments, task.Comment{ID: "2", Body: merge.ProgressPrefix + string(b)})
			}
			got, err := releaseEvidence(context.Background(), ledger, "OF-461", 42, releaseSHA, "owner/repo", pr)
			if name != "manual" && name != "handoff" && name != "known main" {
				if err == nil {
					t.Fatalf("accepted %s: %+v", name, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.TicketID != "canonical" || got.Head != strings.Repeat("a", 40) || got.MergeSHA != releaseSHA || got.Phase != "merged" || got.Deployment != nil {
				t.Fatalf("receipt=%+v", got)
			}
			if name == "manual" && got.QueueID != "" {
				t.Fatalf("invented queue: %+v", got)
			}
			if name == "handoff" && (got.QueueID != "frozen" || got.Corrections != 2 || got.Infra != 3 || got.Attempt != 4 || got.CatchupRounds != 5) {
				t.Fatalf("lost frozen identity/budget: %+v", got)
			}
			if name == "known main" {
				if got.Main != strings.Repeat("b", 40) || got.MainUnknown || got.Corrections != 3 || got.Infra != 4 || got.Attempt != 5 || got.CatchupRounds != 6 {
					t.Fatalf("lost prior progress: %+v", got)
				}
			} else if got.Main != "" || !got.MainUnknown {
				t.Fatalf("invented main: %+v", got)
			}
		})
	}
}
