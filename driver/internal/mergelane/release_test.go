package mergelane

import (
	"context"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"testing"
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
