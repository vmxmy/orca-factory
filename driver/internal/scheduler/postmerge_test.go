package scheduler

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type postmergeOrca struct {
	fakeOrca
	linear *mergeLinear
	fail   string
	calls  []string
}

func (o *postmergeOrca) check(step string) error {
	if o.linear.issue.State.Name != "Done" {
		return fmt.Errorf("cleanup before Done")
	}
	o.calls = append(o.calls, step)
	if o.fail == step {
		return fmt.Errorf("injected %s failure", step)
	}
	return nil
}
func (o *postmergeOrca) WorkerList(_ context.Context, opts orca.ListOptions) (orca.Receipt, error) {
	if err := o.check("list"); err != nil {
		return nil, err
	}
	if opts.Run != "run_1" || !opts.IncludeRemote {
		return nil, fmt.Errorf("wrong run: %+v", opts)
	}
	return receipt(`{"workers":[{"dispatchId":"ctx_done","dispatchStatus":"completed","terminalState":"reclaimable"},{"dispatchId":"ctx_failed","dispatchStatus":"failed","terminalState":"reclaimable"},{"dispatchId":"ctx_active","dispatchStatus":"running"},{"dispatchId":"ctx_gone","dispatchStatus":"completed","terminalState":"released"}]}`), nil
}
func (o *postmergeOrca) WorkerRelease(_ context.Context, request, dispatch string) (orca.Receipt, error) {
	if request != orca.RequestID("ENG-1", "merge-lane", "release-"+dispatch, 1) {
		return nil, fmt.Errorf("unstable release request")
	}
	return nil, o.check("release:" + dispatch)
}
func (o *postmergeOrca) WorktreeSet(_ context.Context, card orca.BoardCard) (orca.Receipt, error) {
	if card.Status != "completed" || card.Comment != "driver: 已合并 · PR #1\nissue: ENG-1" || card.PR != 1 || card.Path == "" {
		return nil, fmt.Errorf("wrong board write")
	}
	return nil, o.check("board")
}
func TestPostmergeCleanupPreservesDone(t *testing.T) {
	for _, failure := range []string{"", "list", "release:ctx_done", "board", "lookup", "delete", "comment", "done"} {
		t.Run(failure, func(t *testing.T) {
			f := newMergeFixture(t)
			o := &postmergeOrca{linear: f.linear, fail: failure}
			o.automations = receipt(`{"automations":[]}`)
			f.runtime.Orca = o
			f.linear.failDone = failure == "done"
			f.linear.failFinal = failure == "comment"
			var gitCalls []string
			f.runtime.Run = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if len(s.Args) > 0 && (s.Args[0] == "ls-remote" || s.Args[0] == "push") {
					if f.linear.issue.State.Name != "Done" {
						t.Fatal("branch cleanup before Done")
					}
					call := strings.Join(s.Args, " ")
					gitCalls = append(gitCalls, call)
					if s.Args[0] == "ls-remote" {
						if failure == "lookup" {
							return nil, fmt.Errorf("lookup failed")
						}
						return []byte(f.head + "\trefs/heads/agent/ENG-1/executor\n"), nil
					}
					if failure == "delete" {
						return nil, fmt.Errorf("delete failed")
					}
					return nil, nil
				}
				return runx.Run(ctx, s)
			}
			err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
			if failure == "done" {
				if err == nil || len(o.calls) != 0 || len(gitCalls) != 0 {
					t.Fatalf("cleanup on unconfirmed Done: err=%v calls=%v git=%v", err, o.calls, gitCalls)
				}
				return
			}
			if f.puts != 1 || f.linear.issue.State.Name != "Done" || (failure != "comment" && err != nil) {
				t.Fatalf("cleanup changed completion: puts=%d state=%s err=%v", f.puts, f.linear.issue.State.Name, err)
			}
			want := []string{"list", "release:ctx_done", "release:ctx_failed", "board"}
			if failure == "list" {
				want = []string{"list", "board"}
			}
			if !reflect.DeepEqual(o.calls, want) {
				t.Fatalf("cleanup calls=%v want=%v", o.calls, want)
			}
			wantGit := []string{"ls-remote --heads origin refs/heads/agent/ENG-1/executor", "push origin --delete refs/heads/agent/ENG-1/executor"}
			if failure == "lookup" {
				wantGit = wantGit[:1]
			}
			if !reflect.DeepEqual(gitCalls, wantGit) {
				t.Fatalf("git=%v want=%v", gitCalls, wantGit)
			}
			if failure != "comment" && !strings.Contains(strings.Join(f.linear.bodies, "\n"), "已合并: PR #1 squash "+strings.Repeat("a", 40)) {
				t.Fatal("lost published merge comment")
			}
		})
	}
}
