package release

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Repository reads a fixed fetched main, never the user's checkout or index.
// API receipts locate published tags; git supplies complete path history so the
// compare API's capped file list cannot hide changes or rename source paths.
type Repository struct {
	Path, Name, Main string
	EnvValues        map[string]string
	Run              func(context.Context, runx.Spec) ([]byte, error)
	GitHub           *github.Client
	tags             []Tag
}
type Tag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}
type Comparison struct {
	BaseCommit struct {
		SHA string `json:"sha"`
	} `json:"base_commit"`
	Status       string `json:"status"`
	TotalCommits int    `json:"total_commits"`
	Commits      []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	} `json:"commits"`
}

func (r *Repository) command(ctx context.Context, dir, exe string, args ...string) ([]byte, error) {
	run := r.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: exe, Dir: dir, Args: args, Timeout: time.Minute})
}
func (r *Repository) git(ctx context.Context, args ...string) ([]byte, error) {
	return r.command(ctx, r.Path, "git", args...)
}
func (r *Repository) api(ctx context.Context, endpoint string, out any) error {
	raw, err := r.command(ctx, r.Path, "gh", "api", "--paginate", "--slurp", "repos/"+r.Name+"/"+endpoint)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (r *Repository) Snapshot(ctx context.Context) (Source, error) {
	copy := *r
	remote, err := r.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return nil, err
	}
	origin := strings.TrimSuffix(strings.TrimSpace(string(remote)), ".git")
	if origin != "https://github.com/"+r.Name && origin != "git@github.com:"+r.Name {
		return nil, errors.New("release origin does not match configured repository")
	}
	if _, err = r.git(ctx, "fetch", "--no-tags", "origin", "main"); err != nil {
		return nil, err
	}
	raw, err := r.git(ctx, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	copy.Main = strings.TrimSpace(string(raw))
	var pages [][]Tag
	if err = r.api(ctx, "tags?per_page=100", &pages); err != nil {
		return nil, err
	}
	copy.tags = nil
	for _, page := range pages {
		copy.tags = append(copy.tags, page...)
	}
	return &copy, nil
}
func (r *Repository) Plugins(ctx context.Context) ([]string, error) {
	raw, err := r.git(ctx, "ls-tree", "-r", "--name-only", r.Main, "--", "mods/")
	if err != nil {
		return nil, err
	}
	var plugins []string
	for _, p := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(p, "/")
		if len(parts) == 4 && parts[2] == ".claude-plugin" && parts[3] == "plugin.json" {
			plugins = append(plugins, parts[1])
		}
	}
	sort.Strings(plugins)
	return plugins, nil
}
func (r *Repository) Env(_ context.Context, key string) (string, error) { return r.EnvValues[key], nil }
func (r *Repository) LatestTag(_ context.Context, plugin string) (string, error) {
	best := ""
	var version [3]uint64
	for _, tag := range r.tags {
		raw, ok := strings.CutPrefix(tag.Name, plugin+"-v")
		if !ok || !versionPattern.MatchString(raw) {
			continue
		}
		var v [3]uint64
		for i, s := range strings.Split(raw, ".") {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return "", err
			}
			v[i] = n
		}
		if best == "" || v[0] > version[0] || v[0] == version[0] && v[1] > version[1] || v[0] == version[0] && v[1] == version[1] && v[2] > version[2] {
			best = tag.Name
			version = v
		}
	}
	return best, nil
}
func (r *Repository) CommitsSince(ctx context.Context, path, tag string) ([]Commit, error) {
	revision := r.Main
	if tag != "" {
		var pages []Comparison
		if err := r.api(ctx, "compare/"+tag+"..."+r.Main+"?per_page=100", &pages); err != nil {
			return nil, err
		}
		if len(pages) == 0 || (pages[0].Status != "ahead" && pages[0].Status != "identical") {
			return nil, errors.New("release baseline is not on main")
		}
		sha := ""
		for _, t := range r.tags {
			if t.Name == tag {
				sha = t.Commit.SHA
			}
		}
		if sha == "" {
			return nil, errors.New("release tag missing")
		}
		if _, err := r.git(ctx, "fetch", "--no-tags", "origin", sha); err != nil {
			return nil, err
		}
		if _, err := r.git(ctx, "merge-base", "--is-ancestor", sha, r.Main); err != nil {
			return nil, err
		}
		revision = sha + ".." + r.Main
	}
	raw, err := r.git(ctx, "log", "--format=%s%x00%b%x00", revision, "--", path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(raw), "\x00")
	var commits []Commit
	for i := 0; i+1 < len(parts); i += 2 {
		commits = append(commits, Commit{Subject: strings.TrimLeft(parts[i], "\n"), Body: parts[i+1]})
	}
	return commits, nil
}
func (r *Repository) LastMerge(ctx context.Context, path string) (time.Time, error) {
	raw, err := r.git(ctx, "log", "-1", "--first-parent", "--format=%ct", r.Main, "--", path)
	if err != nil {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return time.Unix(n, 0), err
}
func (r *Repository) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// ls-tree distinguishes absence from permission/network/command failure.
	raw, err := r.git(ctx, "ls-tree", r.Main, "--", path)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fs.ErrNotExist
	}
	if !strings.HasPrefix(string(raw), "100644 blob ") && !strings.HasPrefix(string(raw), "100755 blob ") {
		return nil, errors.New("release file must be a regular blob")
	}
	return r.git(ctx, "show", r.Main+":"+path)
}
func (r *Repository) CreateReleasePR(ctx context.Context, req merge.Request, p Plan) (int, error) {
	owner, repo, ok := repository(req.Repository)
	if !ok {
		return 0, errors.New("invalid release repository")
	}
	branch := "release/" + p.Plugin + "-v" + p.Version
	existing, err := r.GitHub.FindOpenPR(ctx, owner, repo, branch)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		return existing.Number, nil
	}
	var pages [][]struct {
		Number int `json:"number"`
	}
	query := url.Values{"head": {owner + ":" + branch}, "state": {"closed"}, "per_page": {"100"}}
	if err = r.api(ctx, "pulls?"+query.Encode(), &pages); err != nil {
		return 0, err
	}
	var recovered []int
	for _, page := range pages {
		for _, row := range page {
			recovered = append(recovered, row.Number)
		}
	}
	if len(recovered) > 1 {
		return 0, errors.New("ambiguous prior release PRs")
	}
	if len(recovered) == 1 {
		prior, err := r.GitHub.GetPR(ctx, owner, repo, recovered[0])
		if err != nil {
			return 0, err
		}
		if !prior.Merged || prior.Head.Ref != branch || prior.Head.Repo.FullName != req.Repository || prior.Base.Ref != "main" || prior.Base.Repo.FullName != req.Repository {
			return 0, errors.New("closed release PR requires recovery")
		}
		return prior.Number, nil
	}
	// A private clone avoids touching the configured checkout, worktrees or index.
	dir, err := os.MkdirTemp("", "driver-release-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if _, err = r.command(ctx, r.Path, "git", "clone", "--no-checkout", "--shared", r.Path, dir); err != nil {
		return 0, err
	}
	git := func(args ...string) ([]byte, error) { return r.command(ctx, dir, "git", args...) }
	if _, err = git("remote", "set-url", "origin", "https://github.com/"+req.Repository+".git"); err != nil {
		return 0, err
	}
	// Plan.Base is persisted with the bytes; retries do not pick up later main.
	if p.Base == "" {
		return 0, errors.New("release plan base missing")
	}
	if _, err = git("checkout", "--detach", p.Base); err != nil {
		return 0, err
	}
	paths := make([]string, 0, len(p.Files))
	for path, raw := range p.Files {
		if !strings.HasPrefix(path, "mods/"+p.Plugin+"/") || strings.Contains(path, "..") {
			return 0, errors.New("invalid release path")
		}
		target := filepath.Join(dir, path)
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return 0, err
		}
		if err = os.WriteFile(target, raw, 0644); err != nil {
			return 0, err
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if _, err = git(append([]string{"add", "--"}, paths...)...); err != nil {
		return 0, err
	}
	if _, err = git("-c", "user.name=orca-factory", "-c", "user.email=orca-factory@users.noreply.github.com", "commit", "-m", "chore(release): "+p.Plugin+" v"+p.Version); err != nil {
		return 0, err
	}
	// Never force-push an existing release branch. Recover only identical trees.
	remote, err := git("ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return 0, err
	}
	if len(remote) > 0 {
		if _, err = git("fetch", "origin", "refs/heads/"+branch); err != nil {
			return 0, err
		}
		if _, err = git("diff", "--exit-code", "HEAD", "FETCH_HEAD", "--"); err != nil {
			return 0, errors.New("release branch differs from reserved plan")
		}
	} else {
		if _, err = git("push", "origin", "HEAD:refs/heads/"+branch); err != nil {
			return 0, err
		}
	}
	pr, err := r.GitHub.CreatePR(ctx, owner, repo, github.CreatePRRequest{Head: branch, Base: "main", Title: "chore(release): " + p.Plugin + " v" + p.Version, Body: p.Section})
	return pr.Number, err
}

// RecoveryPublisher checks durable remote receipts before retrying partial writes.
func (r *Repository) CreateTag(ctx context.Context, owner, repo, tag, sha string) error {
	var pages [][]Tag
	if err := r.api(ctx, "tags?per_page=100", &pages); err != nil {
		return err
	}
	for _, page := range pages {
		for _, t := range page {
			if t.Name == tag {
				if t.Commit.SHA != sha {
					return errors.New("published tag points elsewhere")
				}
				return nil
			}
		}
	}
	return r.GitHub.CreateTag(ctx, owner, repo, tag, sha)
}
func (r *Repository) CreateRelease(ctx context.Context, owner, repo string, input github.ReleaseRequest) (github.Release, error) {
	var pages [][]github.Release
	if err := r.api(ctx, "releases?per_page=100", &pages); err != nil {
		return github.Release{}, err
	}
	for _, page := range pages {
		for _, p := range page {
			if p.TagName == input.TagName {
				return p, nil
			}
		}
	}
	return r.GitHub.CreateRelease(ctx, owner, repo, input)
}

// CompletePlan preserves Source's published interface while pinning production plans.
func (r *Repository) CompletePlan(ctx context.Context, p *Plan) error {
	p.Base = r.Main
	path := "mods/" + p.Plugin + "/hooks/register.tsx"
	raw, err := r.ReadFile(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pattern := regexp.MustCompile(`(?m)(\bconst MOD_VERSION\s*=\s*['"])([0-9]+\.[0-9]+\.[0-9]+)(['"])`)
	matches := pattern.FindAllSubmatchIndex(raw, -1)
	if len(matches) == 0 {
		return nil
	}
	if len(matches) != 1 {
		return errors.New("ambiguous MOD_VERSION")
	}
	m := matches[0]
	updated := append([]byte(nil), raw[:m[4]]...)
	updated = append(updated, []byte(p.Version)...)
	updated = append(updated, raw[m[5]:]...)
	p.Files[path] = updated
	return nil
}
