package mergelane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type publisher struct{ tags, releases int }

func (p *publisher) CreateTag(context.Context, string, string, string, string) error {
	p.tags++
	return nil
}
func (p *publisher) CreateRelease(context.Context, string, string, github.ReleaseRequest) (github.Release, error) {
	p.releases++
	return github.Release{}, nil
}

type writer struct{}

func (writer) CreateReleasePR(context.Context, merge.Request, release.Plan) (int, error) {
	return 8, nil
}
func TestReleaseSharesFIFOAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	feature := openPR()
	rel := openPR()
	rel.Number = 8
	gh := &fakeGitHub{prs: []github.PullRequest{feature, rel}}
	l, _, tk, _, ticket := lane(t, gh)
	pub := &publisher{}
	l.Publisher = pub
	if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
		t.Fatal(err)
	}
	// Recover the durable arrival order, even with no feature ticket in this tick.
	restored := *l
	restored.entries = nil
	restored.loaded = false
	l = &restored
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 0 || pub.tags != 0 {
		t.Fatal("release overtook waiting feature")
	}
	gh.runs = green()
	if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if len(tk.finished) != 1 || pub.tags != 0 {
		t.Fatal("feature must finish first")
	}
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 2 || pub.tags != 1 || pub.releases != 1 {
		t.Fatalf("merges=%v publisher=%+v", gh.merges, pub)
	}
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if pub.tags != 1 || pub.releases != 1 {
		t.Fatal("publication repeated")
	}
}

func TestReleaseGateUsesRepositoryGate(t *testing.T) {
	l, _, _, _, _ := lane(t, &fakeGitHub{})
	r := merge.Request{Repository: "vmxmy/orca-factory", Path: t.TempDir(), Number: 8}
	l.entries = []laneEntry{{Request: r, Release: &release.Pending{Request: r, Plan: release.Plan{Plugin: "demo", Version: "1.0.1", Files: map[string][]byte{"mods/demo/.claude-plugin/plugin.json": []byte(`{"version":"1.0.1"}`), "mods/demo/CHANGELOG.md": []byte("## 1.0.1\n")}}}}}
	var spec string
	l.Exec = func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != filepath.Join(r.Path, "scripts/agent-gates/gate.sh") || len(s.Args) != 2 || s.Args[0] != "/validated" {
			t.Fatalf("gate command=%+v", s)
		}
		b, err := os.ReadFile(s.Args[1])
		if err != nil {
			t.Fatal(err)
		}
		spec = string(b)
		return nil, nil
	}
	if err := l.Run(context.Background(), r, "/validated"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec, "SCOPE: mods/demo/.claude-plugin/plugin.json mods/demo/CHANGELOG.md") || !strings.Contains(spec, "CHECK: git diff --check") {
		t.Fatal(spec)
	}
}

type interruptedPublisher struct {
	publisher
	fail bool
}

func (p *interruptedPublisher) CreateRelease(ctx context.Context, owner, repo string, r github.ReleaseRequest) (github.Release, error) {
	if p.fail {
		p.fail = false
		return github.Release{}, errors.New("lost publication")
	}
	return p.publisher.CreateRelease(ctx, owner, repo, r)
}
func TestReleasePublicationRecovery(t *testing.T) {
	ctx := context.Background()
	pr := openPR()
	pr.Number = 8
	gh := &fakeGitHub{prs: []github.PullRequest{pr}, runs: green()}
	l, _, _, _, _ := lane(t, gh)
	pub := &interruptedPublisher{fail: true}
	l.Publisher = pub
	if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Process(ctx, nil); err == nil {
		t.Fatal("publication error disappeared")
	}
	pr.Merged = true
	pr.State = "closed"
	pr.MergeCommitSHA = sha
	gh.prs = []github.PullRequest{pr}
	restored := *l
	restored.entries = nil
	restored.loaded = false
	l = &restored
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 1 || pub.releases != 1 {
		t.Fatalf("merges=%v published=%d", gh.merges, pub.releases)
	}
	pending, err := l.HasRelease("vmxmy/orca-factory", "demo")
	if err != nil || pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}
