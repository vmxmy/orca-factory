package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

type PRClient interface {
	FindPRs(context.Context, string, string, string) ([]github.PullRequest, error)
	CreatePR(context.Context, string, string, github.CreatePRRequest) (github.PullRequest, error)
}

// Writer uses a detached worktree and a deterministic remote branch. Existing
// branches and PRs are recovered, never rewritten or force-pushed.
type Writer struct {
	Source *RepositorySource
	GitHub PRClient
}

func (w *Writer) CreateReleasePR(ctx context.Context, r merge.Request, p Plan) (int, error) {
	owner, repo, ok := repository(r.Repository)
	if !ok {
		return 0, errors.New("invalid repository")
	}
	branch := "agent/release/" + p.Plugin + "-v" + p.Version
	prs, err := w.GitHub.FindPRs(ctx, owner, repo, branch)
	if err != nil {
		return 0, err
	}
	for _, pr := range prs {
		if pr.State == "open" || pr.Merged {
			return pr.Number, nil
		}
		return 0, errors.New("release PR closed; human recovery required")
	}
	ref := "refs/heads/" + branch
	remote, err := w.Source.run(ctx, "git", "ls-remote", "--heads", "origin", ref)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(string(remote)) == "" {
		root := filepath.Join(r.Path, ".omc", "tmp")
		if err = os.MkdirAll(root, 0700); err != nil {
			return 0, err
		}
		dir, err := os.MkdirTemp(root, "release-")
		if err != nil {
			return 0, err
		}
		defer os.RemoveAll(dir)
		if _, err = w.Source.run(ctx, "git", "worktree", "add", "--detach", dir, w.Source.Head); err != nil {
			return 0, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			w.Source.run(cleanup, "git", "worktree", "remove", "--force", dir)
		}()
		var paths []string
		for path, b := range p.Files {
			if path != "mods/"+p.Plugin+"/.claude-plugin/plugin.json" && path != "mods/"+p.Plugin+"/CHANGELOG.md" {
				return 0, errors.New("unexpected release path")
			}
			target := filepath.Join(dir, path)
			if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return 0, err
			}
			if err = os.WriteFile(target, b, 0644); err != nil {
				return 0, err
			}
			paths = append(paths, path)
		}
		sort.Strings(paths)
		local := *w.Source
		local.Repository.Path = dir
		if _, err = local.run(ctx, "git", append([]string{"add", "--"}, paths...)...); err != nil {
			return 0, err
		}
		if _, err = local.run(ctx, "git", "-c", "core.hooksPath=/dev/null", "commit", "-m", fmt.Sprintf("chore(%s): release %s", p.Plugin, p.Version)); err != nil {
			return 0, err
		}
		if _, err = local.run(ctx, "git", "push", "origin", "HEAD:"+ref); err != nil {
			return 0, err
		}
	}
	pr, err := w.GitHub.CreatePR(ctx, owner, repo, github.CreatePRRequest{Head: branch, Base: "main", Title: fmt.Sprintf("chore(%s): release %s", p.Plugin, p.Version), Body: p.Section})
	return pr.Number, err
}

// RecoveringPublisher reads before every write, so a tag created before a crash
// or a lost release response does not cause blind retries or duplicate releases.
type RecoveringPublisher struct {
	Source *RepositorySource
	Client Publisher
}

func (p *RecoveringPublisher) CreateTag(ctx context.Context, owner, repo, tag, sha string) error {
	for page := 1; ; page++ {
		var tags []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := p.Source.API(ctx, fmt.Sprintf("repos/%s/%s/tags?per_page=100&page=%d", owner, repo, page), &tags); err != nil {
			return err
		}
		for _, t := range tags {
			if t.Name == tag {
				if t.Commit.SHA != sha {
					return errors.New("release tag points to another commit")
				}
				return nil
			}
		}
		if len(tags) < 100 {
			break
		}
	}
	return p.Client.CreateTag(ctx, owner, repo, tag, sha)
}
func (p *RecoveringPublisher) CreateRelease(ctx context.Context, owner, repo string, r github.ReleaseRequest) (github.Release, error) {
	for page := 1; ; page++ {
		var releases []github.Release
		if err := p.Source.API(ctx, fmt.Sprintf("repos/%s/%s/releases?per_page=100&page=%d", owner, repo, page), &releases); err != nil {
			return github.Release{}, err
		}
		for _, existing := range releases {
			if existing.TagName == r.TagName {
				return existing, nil
			}
		}
		if len(releases) < 100 {
			break
		}
	}
	return p.Client.CreateRelease(ctx, owner, repo, r)
}
