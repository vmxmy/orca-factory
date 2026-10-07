package scheduler

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type scopeBackend struct {
	*fakeBackend
	files []string
	found bool
	err   error
	reads int
}

func (f *scopeBackend) ActualChanges(context.Context, Ticket) ([]string, bool, error) {
	f.reads++
	return f.files, f.found, f.err
}

func TestScopeActualChanges(t *testing.T) {
	t.Run("snapshot scope read failures", func(t *testing.T) {
		for _, failure := range []string{"orca failure", "invalid JSON", "ok false"} {
			t.Run(failure, func(t *testing.T) {
				lc := &fakeLinear{}
				for _, id := range []string{"bad", "healthy"} {
					lc.issues = append(lc.issues, linear.Issue{ID: id, Identifier: id, Project: linear.NamedID{ID: id}, State: linear.NamedID{Name: "Todo"}, Labels: linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{{Name: "ready-for-agent"}}}})
				}
				reads := 0
				r := &Runtime{Clients: map[string]LinearClient{"a": lc, "b": lc}, Run: func(context.Context, runx.Spec) ([]byte, error) {
					reads++
					if reads == 1 {
						switch failure {
						case "orca failure":
							return nil, errors.New("orca unavailable")
						case "invalid JSON":
							return []byte(`{`), nil
						case "ok false":
							return []byte(`{"ok":false,"result":{"issue":{"description":"## SCOPE\nsrc/*"}}}`), nil
						}
					}
					return []byte(`{"ok":true,"result":{"issue":{"description":"## SCOPE\ndocs/*"}}}`), nil
				}}
				tickets, err := r.Snapshot(context.Background(), []config.Repository{{Path: "a", Env: map[string]string{"AGENT_LINEAR_PROJECT": "bad"}}, {Path: "b", Env: map[string]string{"AGENT_LINEAR_PROJECT": "healthy"}}})
				if err != nil {
					t.Fatalf("scope failure aborted snapshot: %v", err)
				}
				if len(tickets) != 2 || tickets[0].ID != "bad" || tickets[1].ID != "healthy" || len(tickets[0].Scope) != 0 || !reflect.DeepEqual(tickets[1].Scope, []string{"docs/*"}) || !tickets[0].Candidate || !tickets[1].Candidate {
					t.Fatalf("snapshot lost tickets or scope: %+v", tickets)
				}
			})
		}
	})
	for _, tc := range []struct {
		name     string
		files    []string
		found    bool
		err      error
		disabled bool
		want     int
	}{
		{"disjoint actual files", []string{"docs/readme.md"}, true, nil, false, 1},
		{"overlapping actual files", []string{"src/deep/file.go"}, true, nil, false, 0},
		{"no branches", nil, false, nil, false, 0},
		{"fetch failure", nil, false, errors.New("fetch failed"), false, 0},
		{"diff failure discards partial files", []string{"docs/readme.md"}, true, errors.New("diff failed"), false, 0},
		{"disabled", []string{"src/file.go"}, true, nil, true, 1},
		{"empty actual diff", []string{}, true, nil, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &scopeBackend{fakeBackend: &fakeBackend{}, files: tc.files, found: tc.found, err: tc.err}
			s := setup(f.fakeBackend)
			s.Backend = f
			if tc.disabled {
				s.Config.Repositories[1].Env = map[string]string{"AGENT_SCOPE_AVOID": "0"}
			}
			f.tickets = []Ticket{
				{ID: "live", Identifier: "ZII-1", Repo: "b", Owner: machine.OwnerMerge, UpdatedAt: s.Now(), Scope: []string{"src/*"}},
				{ID: "new", Repo: "b", Candidate: true, Scope: []string{"src/*"}},
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.launched) != tc.want || len(f.actions) != tc.want {
				t.Fatalf("launched=%v actions=%v want=%d", f.launched, f.actions, tc.want)
			}
			wantReads := 1
			if tc.disabled {
				wantReads = 0
			}
			if f.reads != wantReads {
				t.Fatalf("branch reads=%d want=%d", f.reads, wantReads)
			}
		})
	}
	t.Run("glob semantics", func(t *testing.T) {
		for _, tc := range []struct {
			a, b string
			want bool
		}{
			{"src/*", "src/deep/file.go", true}, {"src/deep/file.go", "src/*", true},
			{"src", "src/deep/file.go", true}, {"src/deep", "src/", true},
			{"src/[ab]?.go", "src/a1.go", true}, {"src/*", "docs/*", false},
			{"src", "src-other/file", false}, {"same/*", "same/*", true},
		} {
			if got := scopesOverlap([]string{tc.a}, []string{tc.b}); got != tc.want {
				t.Errorf("overlap(%q,%q)=%v", tc.a, tc.b, got)
			}
		}
	})
	t.Run("parse scope", func(t *testing.T) {
		if got := declaredScope("# Brief\n## SCOPE\n\n src/* docs/a.md\nignored/*"); !reflect.DeepEqual(got, []string{"src/*", "docs/a.md"}) {
			t.Fatalf("scope=%v", got)
		}
		if got := declaredScope("## SCOPED\nwrong/*\n## SCOPE\n## CHECK\ncommand"); len(got) != 0 {
			t.Fatalf("scope=%v", got)
		}
	})
	t.Run("runtime union and failures", func(t *testing.T) {
		for _, fail := range []string{"", "fetch", "diff", "for-each-ref", "no branches"} {
			t.Run(fail, func(t *testing.T) {
				calls := 0
				r := &Runtime{Run: func(ctx context.Context, spec runx.Spec) ([]byte, error) {
					calls++
					if _, ok := ctx.Deadline(); !ok || spec.Timeout <= 0 || spec.Path != "git" || spec.Dir != "/repo" {
						t.Fatalf("unbounded/wrong command: %+v", spec)
					}
					if spec.Args[0] == fail {
						return nil, errors.New("failed")
					}
					switch spec.Args[0] {
					case "fetch":
						return nil, nil
					case "for-each-ref":
						if fail == "no branches" {
							return nil, nil
						}
						return []byte("refs/remotes/origin/agent/ZII-1/a\nrefs/remotes/origin/agent/ZII-1/b\n"), nil
					case "diff":
						if !strings.Contains(strings.Join(spec.Args, " "), "origin/main...") {
							t.Fatal("not merge-base diff")
						}
						if calls == 3 {
							return []byte("src/a.go\x00docs/x.md\x00"), nil
						}
						return []byte("src/a.go\x00src/b.go\x00"), nil
					}
					t.Fatalf("unexpected git command: %v", spec.Args)
					return nil, nil
				}}
				got, found, err := r.ActualChanges(context.Background(), Ticket{Repo: "/repo", Identifier: "ZII-1"})
				if fail == "" {
					if err != nil || !found || !reflect.DeepEqual(got, []string{"docs/x.md", "src/a.go", "src/b.go"}) {
						t.Fatalf("files=%v found=%v err=%v", got, found, err)
					}
				} else if fail == "no branches" {
					if err != nil || found {
						t.Fatalf("found=%v err=%v", found, err)
					}
				} else if err == nil {
					t.Fatal("lost git failure")
				}
			})
		}
	})
}
