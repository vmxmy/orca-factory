package release

import (
	"bytes"
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

func (w *Writer) CreateReleasePR(ctx context.Context, r merge.Request, p Plan) (number int, resultErr error) {
	owner, repo, ok := repository(r.Repository)
	if !ok {
		return 0, errors.New("invalid repository")
	}
	if !pluginPattern.MatchString(p.Plugin) || !versionPattern.MatchString(p.Version) || len(p.Files) != 2 {
		return 0, errors.New("invalid release plan")
	}
	for path := range p.Files {
		if path != "mods/"+p.Plugin+"/.claude-plugin/plugin.json" && path != "mods/"+p.Plugin+"/CHANGELOG.md" {
			return 0, errors.New("unexpected release path")
		}
	}
	branch := "agent/release/" + p.Plugin + "-v" + p.Version
	prs, err := w.GitHub.FindPRs(ctx, owner, repo, branch)
	if err != nil {
		return 0, err
	}
	for _, pr := range prs {
		if pr.State == "open" || pr.Merged {
			if err := w.verify(ctx, "refs/heads/"+branch, p); err != nil {
				return 0, err
			}
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

		if _, err = w.Source.run(ctx, "git", "worktree", "add", "--detach", dir, w.Source.Head); err != nil {
			os.Remove(dir)
			return 0, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := w.Source.run(cleanup, "git", "worktree", "remove", "--force", dir); err != nil {
				resultErr = errors.Join(resultErr, errors.New("release worktree cleanup failed"))
			}
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
			if info, e := os.Lstat(target); e == nil && !info.Mode().IsRegular() {
				return 0, errors.New("release target is not a regular file")
			} else if e != nil && !errors.Is(e, os.ErrNotExist) {
				return 0, e
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
		if _, err = local.run(ctx, "git", "commit", "-m", fmt.Sprintf("chore(%s): release %s", p.Plugin, p.Version)); err != nil {
			return 0, err
		}
		if _, err = local.run(ctx, "git", "push", "origin", "HEAD:"+ref); err != nil {
			return 0, err
		}
	} else if err = w.verify(ctx, ref, p); err != nil {
		return 0, err
	}
	pr, err := w.GitHub.CreatePR(ctx, owner, repo, github.CreatePRRequest{Head: branch, Base: "main", Title: fmt.Sprintf("chore(%s): release %s", p.Plugin, p.Version), Body: p.Section})
	return pr.Number, err
}

// A crash between remote branch/PR creation and queue persistence may leave a
// prior plan on GitHub. Never publish new notes for a different recovered PR.
func (w *Writer) verify(ctx context.Context, ref string, p Plan) error {
	if _, err := w.Source.run(ctx, "git", "fetch", "--no-tags", "origin", ref); err != nil {
		return err
	}
	for path, want := range p.Files {
		got, err := w.Source.run(ctx, "git", "show", "FETCH_HEAD:"+path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return errors.New("recovered release differs from plan; human recovery required")
		}
	}
	return nil
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
