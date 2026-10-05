package release

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

func TestBump(t *testing.T) {
	for _, tt := range []struct{ current, subject, body, want string }{
		{"0.3.15", "feat: new", "", "0.4.0"}, {"1.2.3", "feat(ui): new", "", "1.3.0"},
		{"1.2.3", "fix: bug", "", "1.2.4"}, {"1.2.3", "docs: text", "", "1.2.4"},
		{"0.3.15", "feat!: break", "", "0.4.0"}, {"1.2.3", "fix(x)!: break", "", "2.0.0"},
		{"1.2.3", "chore: new", "BREAKING CHANGE: removed API", "2.0.0"},
		{"0.3.15", "fix: new", "BREAKING CHANGE: removed API", "0.4.0"},
		{"1.2.3", "feature: text", "", "1.2.4"},
	} {
		t.Run(tt.subject+tt.current+tt.body, func(t *testing.T) {
			got, err := Bump(tt.current, []Commit{{Subject: tt.subject, Body: tt.body}, {Subject: "fix: other"}})
			if err != nil || got != tt.want {
				t.Fatalf("got %q %v, want %s", got, err, tt.want)
			}
		})
	}
	for _, v := range []string{"", "1.2", "01.2.3", "1.2.-3", "v1.2.3"} {
		if _, err := Bump(v, nil); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestGenerate(t *testing.T) {
	manifest := []byte("{\n  \"name\": \"demo\",\n  \"nested\": {\"version\":\"unchanged\"},\n  \"version\" : \"0.3.15\",\n  \"description\": \"中文\"\n}\n")
	for _, old := range []string{"", "# Changelog\n\nold\n"} {
		p, err := Generate("demo", manifest, []byte(old), []Commit{{Subject: "feat: new"}, {Subject: "fix: bug"}})
		if err != nil {
			t.Fatal(err)
		}
		section := "## 0.4.0\n\n- feat: new\n- fix: bug\n"
		prefix := old
		if prefix != "" {
			prefix += "\n"
		}
		want := map[string][]byte{"mods/demo/.claude-plugin/plugin.json": []byte(strings.Replace(string(manifest), `"0.3.15"`, `"0.4.0"`, 1)), "mods/demo/CHANGELOG.md": []byte(prefix + section)}
		if !reflect.DeepEqual(p.Files, want) || p.Section != section || p.Version != "0.4.0" || p.Plugin != "demo" {
			t.Fatalf("unexpected plan: %+v", p)
		}
	}
	for _, doc := range []string{`{}`, `{"version":2}`, `{"version":"1.0.0","version":"2.0.0"}`, `[]`, `{"version":"1.0.0"} garbage`} {
		if _, err := Generate("demo", []byte(doc), nil, nil); err == nil {
			t.Errorf("accepted %s", doc)
		}
	}
	if _, err := Generate("../escape", manifest, nil, nil); err == nil {
		t.Fatal("accepted traversal")
	}
}

type fakeSource struct {
	now        time.Time
	quiet, tag string
	commits    []Commit
	age        time.Duration
	queries    []string
	fail       bool
}

func (f *fakeSource) Plugins(context.Context) ([]string, error)   { return []string{"demo", "idle"}, nil }
func (f *fakeSource) Env(context.Context, string) (string, error) { return f.quiet, nil }
func (f *fakeSource) LatestTag(_ context.Context, p string) (string, error) {
	if p == "idle" {
		return "", nil
	}
	return f.tag, nil
}
func (f *fakeSource) CommitsSince(_ context.Context, path, tag string) ([]Commit, error) {
	f.queries = append(f.queries, path+"@"+tag)
	if path == "mods/idle" {
		return nil, nil
	}
	return f.commits, nil
}
func (f *fakeSource) LastMerge(context.Context, string) (time.Time, error) {
	return f.now.Add(-f.age), nil
}
func (f *fakeSource) ReadFile(_ context.Context, path string) ([]byte, error) {
	if f.fail {
		return nil, errors.New("secret-token")
	}
	if strings.HasSuffix(path, "CHANGELOG.md") {
		return nil, fs.ErrNotExist
	}
	return []byte(`{"version":"0.3.15"}`), nil
}
func TestScan(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, quiet, tag string
		age              time.Duration
		commits          []Commit
		want             int
	}{
		{"too soon", "", "demo-v0.3.15", 29 * time.Minute, []Commit{{Subject: "fix: bug"}}, 0},
		{"boundary", "", "demo-v0.3.15", 30 * time.Minute, []Commit{{Subject: "fix: bug"}}, 0},
		{"ready", "", "demo-v0.3.15", 31 * time.Minute, []Commit{{Subject: "fix: bug"}}, 1},
		{"never released", "", "", 31 * time.Minute, []Commit{{Subject: "feat: new"}}, 1},
		{"no changes", "", "demo-v0.3.15", time.Hour, nil, 0},
		{"configured", "60", "demo-v0.3.15", 31 * time.Minute, []Commit{{Subject: "fix: bug"}}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{now: now, quiet: tt.quiet, tag: tt.tag, age: tt.age, commits: tt.commits}
			plans, err := Scan(context.Background(), src, func() time.Time { return now })
			if err != nil || len(plans) != tt.want {
				t.Fatalf("plans=%v err=%v", plans, err)
			}
			if !reflect.DeepEqual(src.queries, []string{"mods/demo@" + tt.tag, "mods/idle@"}) {
				t.Fatalf("queries: %v", src.queries)
			}
		})
	}
	for _, quiet := range []string{"-1", "bad", "99999999999999999"} {
		src := &fakeSource{quiet: quiet}
		if _, err := Scan(context.Background(), src, func() time.Time { return now }); err == nil {
			t.Errorf("accepted quiet %q", quiet)
		}
	}
	src := &fakeSource{now: now, age: time.Hour, commits: []Commit{{Subject: "fix: bug"}}, fail: true}
	if _, err := Scan(context.Background(), src, func() time.Time { return now }); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error %v", err)
	}
}

type fakePR struct {
	plan   Plan
	number int
	err    error
}

func (f *fakePR) CreateReleasePR(_ context.Context, r merge.Request, p Plan) (int, error) {
	f.plan = p
	return f.number, f.err
}

type fakeQueue struct {
	requests []merge.Request
	err      error
}

func (f *fakeQueue) Enqueue(r merge.Request) error { f.requests = append(f.requests, r); return f.err }

type fakePublisher struct {
	calls     []string
	sha, body string
	fail      bool
}

func (f *fakePublisher) CreateTag(_ context.Context, owner, repo, tag, sha string) error {
	f.calls = append(f.calls, owner+"/"+repo+":"+tag)
	f.sha = sha
	if f.fail {
		return errors.New("secret-token")
	}
	return nil
}
func (f *fakePublisher) CreateRelease(_ context.Context, owner, repo string, r github.ReleaseRequest) (github.Release, error) {
	f.calls = append(f.calls, r.TagName)
	f.body = r.Body
	return github.Release{TagName: r.TagName}, nil
}
func TestSubmitAndPublish(t *testing.T) {
	p, err := Generate("demo", []byte(`{"version":"0.3.15"}`), nil, []Commit{{Subject: "feat: new"}})
	if err != nil {
		t.Fatal(err)
	}
	req := merge.Request{Repository: "acme/plugins", Path: "/checkout", DeployWorkflow: "deploy", SmokeCommand: "smoke"}
	pr := &fakePR{number: 42}
	queue := &fakeQueue{}
	pending, err := Submit(context.Background(), pr, queue, req, p)
	if err != nil {
		t.Fatal(err)
	}
	req.Number = 42
	if !reflect.DeepEqual(queue.requests, []merge.Request{req}) || !reflect.DeepEqual(pr.plan, p) {
		t.Fatalf("bad enqueue: %+v", queue.requests)
	}
	pub := &fakePublisher{}
	result := merge.Result{Status: merge.Waiting, Request: req}
	if err := Publish(context.Background(), pub, pending, result); err != nil || len(pub.calls) != 0 {
		t.Fatalf("published before merge: %v", err)
	}
	result.Status = merge.Merged
	result.MergeSHA = "squash-sha"
	if err := Publish(context.Background(), pub, pending, result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pub.calls, []string{"acme/plugins:demo-v0.4.0", "demo-v0.4.0"}) || pub.sha != "squash-sha" || pub.body != "## 0.4.0\n\n- feat: new\n" {
		t.Fatalf("bad publication %+v", pub)
	}
	pub = &fakePublisher{fail: true}
	if err := Publish(context.Background(), pub, pending, result); err == nil || strings.Contains(err.Error(), "secret-token") || len(pub.calls) != 1 {
		t.Fatalf("tag failure ignored %v %+v", err, pub)
	}
	result.Request.Number = 99
	if err := Publish(context.Background(), pub, pending, result); err == nil {
		t.Fatal("accepted wrong PR")
	}
	pr.err = errors.New("secret-token")
	queue.requests = nil
	if _, err := Submit(context.Background(), pr, queue, req, p); err == nil || strings.Contains(err.Error(), "secret-token") || len(queue.requests) != 0 {
		t.Fatalf("PR error ignored: %v", err)
	}
	pr.err = nil
	queue.err = errors.New("queue failure")
	if pending, err := Submit(context.Background(), pr, queue, req, p); err == nil || pending.Request.Number != 42 {
		t.Fatalf("lost PR on enqueue failure: %+v %v", pending, err)
	}
}
