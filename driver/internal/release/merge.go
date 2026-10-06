package release

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// MergeAdapter validates the entire release diff against its reserved bytes.
// Neither a branch name nor green CI alone authorizes arbitrary code to merge.
type MergeAdapter struct {
	Repository *Repository
	Manager    *Manager
}

func (a *MergeAdapter) plan(number int) (Plan, error) {
	rows, err := a.Manager.entries()
	if err != nil {
		return Plan{}, err
	}
	for _, e := range rows {
		if e.Pending.Request.Number == number && !e.Done {
			return e.Pending.Plan, nil
		}
	}
	return Plan{}, errors.New("release receipt missing")
}
func (a *MergeAdapter) Snapshot(ctx context.Context, req merge.Request) (merge.Snapshot, error) {
	r := a.Repository
	owner, repo, _ := repository(req.Repository)
	p, err := a.plan(req.Number)
	if err != nil {
		return merge.Snapshot{}, err
	}
	pr, err := r.GitHub.GetPR(ctx, owner, repo, req.Number)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if pr.Number != req.Number || pr.Head.Ref != "release/"+p.Plugin+"-v"+p.Version || pr.Head.Repo.FullName != req.Repository || pr.Base.Repo.FullName != req.Repository || pr.Base.Ref != "main" {
		return merge.Snapshot{}, errors.New("release PR identity changed")
	}
	main, err := r.GitHub.MainSHA(ctx, owner, repo)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if _, err = r.git(ctx, "fetch", "--no-tags", "origin", pr.Head.SHA, main); err != nil {
		return merge.Snapshot{}, err
	}
	paths, err := r.git(ctx, "diff", "--name-only", "--no-renames", "-z", p.Base, pr.Head.SHA, "--")
	if err != nil {
		return merge.Snapshot{}, err
	}
	names := strings.Split(strings.TrimSuffix(string(paths), "\x00"), "\x00")
	if len(names) != len(p.Files) {
		return merge.Snapshot{}, errors.New("release diff changed")
	}
	for _, path := range names {
		want, ok := p.Files[path]
		if !ok {
			return merge.Snapshot{}, errors.New("release changed unexpected path")
		}
		actual, err := r.git(ctx, "show", pr.Head.SHA+":"+path)
		if err != nil || !bytes.Equal(actual, want) {
			return merge.Snapshot{}, errors.New("release bytes changed")
		}
	}
	if pr.Merged {
		if pr.MergeSHA == "" {
			return merge.Snapshot{}, errors.New("merged release SHA missing")
		}
		return merge.Snapshot{Head: pr.Head.SHA, Main: main, CIHead: pr.Head.SHA, CIGreen: true, MergeableState: "clean"}, nil
	}
	if pr.State != "open" || pr.Draft == nil || *pr.Draft || pr.Mergeable == nil {
		return merge.Snapshot{}, errors.New("release PR not ready")
	}
	// New plugin commits invalidate the frozen changelog/version. Hold the lane
	// for recovery instead of publishing a version that silently omits changes.
	changed, err := r.git(ctx, "diff", "--name-only", p.Base, main, "--", "mods/"+p.Plugin)
	if err != nil {
		return merge.Snapshot{}, err
	}
	if len(changed) > 0 {
		return merge.Snapshot{}, errors.New("plugin changed since release was planned")
	}
	green, err := r.GitHub.MergeChecks(ctx, owner, repo, pr.Head.SHA)
	if err != nil {
		return merge.Snapshot{}, err
	}
	state := pr.MergeableState
	if !*pr.Mergeable {
		state = "dirty"
	}
	return merge.Snapshot{Head: pr.Head.SHA, Main: main, CIHead: pr.Head.SHA, CIGreen: green, MergeableState: state, PRFiles: names}, nil
}
func (a *MergeAdapter) Squash(ctx context.Context, req merge.Request, head, main string) (string, error) {
	fresh, err := a.Snapshot(ctx, req)
	if err != nil {
		return "", err
	}
	if fresh.Main != main {
		return "", merge.ErrMainAdvanced
	}
	if fresh.Head != head || !fresh.CIGreen || fresh.MergeableState != "clean" {
		return "", errors.New("release validation changed")
	}
	owner, repo, _ := repository(req.Repository)
	pr, err := a.Repository.GitHub.GetPR(ctx, owner, repo, req.Number)
	if err != nil {
		return "", err
	}
	if pr.Merged {
		return pr.MergeSHA, nil
	}
	pr, err = a.Repository.GitHub.MergePR(ctx, owner, repo, req.Number, head)
	if err != nil {
		return "", err
	}
	if pr.Head.SHA != head {
		return "", errors.New("release head changed")
	}
	return pr.MergeSHA, nil
}
