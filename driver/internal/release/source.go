package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// RepositorySource pins all reads to one fetched main commit. Git supplies path
// membership and integration timestamps; GitHub supplies tags and commit text.
type RepositorySource struct {
	Repository config.Repository
	Head       string
	Exec       func(context.Context, runx.Spec) ([]byte, error)
}

func (s *RepositorySource) run(ctx context.Context, command string, args ...string) ([]byte, error) {
	run := s.Exec
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: command, Args: args, Dir: s.Repository.Path, Timeout: 2 * time.Minute})
}
func (s *RepositorySource) API(ctx context.Context, endpoint string, out any) error {
	b, err := s.run(ctx, "gh", "api", endpoint)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func (s *RepositorySource) Refresh(ctx context.Context) error {
	if _, _, ok := repository(s.Repository.Env["AGENT_GH_REPO"]); !ok {
		return errors.New("invalid release repository")
	}
	if _, err := s.run(ctx, "git", "fetch", "--no-tags", "origin", "main"); err != nil {
		return err
	}
	b, err := s.run(ctx, "git", "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	s.Head = strings.TrimSpace(string(b))
	b, err = s.run(ctx, "git", "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "false" {
		return errors.New("release scan requires complete history")
	}
	return nil
}
func (s *RepositorySource) Plugins(ctx context.Context) ([]string, error) {
	b, err := s.run(ctx, "git", "ls-tree", "-d", "--name-only", s.Head+":mods")
	if err != nil {
		// An absent mods directory is a repository without plugins, not a failed fetch.
		root, e := s.run(ctx, "git", "ls-tree", "--name-only", s.Head)
		if e != nil {
			return nil, e
		}
		for _, p := range strings.Split(string(root), "\n") {
			if p == "mods" {
				return nil, err
			}
		}
		return nil, nil
	}
	var result []string
	for _, p := range strings.Fields(string(b)) {
		if !pluginPattern.MatchString(p) {
			return nil, errors.New("invalid plugin directory")
		}
		if _, e := s.ReadFile(ctx, "mods/"+p+"/.claude-plugin/plugin.json"); errors.Is(e, fs.ErrNotExist) {
			continue
		} else if e != nil {
			return nil, e
		}
		result = append(result, p)
	}
	sort.Strings(result)
	return result, nil
}
func (s *RepositorySource) Env(_ context.Context, key string) (string, error) {
	return s.Repository.Env[key], nil
}

type tagReceipt struct {
	Name string `json:"name"`
}

func latestVersionTag(tags []tagReceipt, prefix string) string {
	var best string
	var version [3]uint64
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Name, prefix) {
			continue
		}
		m := versionPattern.FindStringSubmatch(strings.TrimPrefix(tag.Name, prefix))
		if m == nil {
			continue
		}
		var v [3]uint64
		valid := true
		for i := range v {
			var err error
			v[i], err = strconv.ParseUint(m[i+1], 10, 64)
			valid = valid && err == nil
		}
		if !valid {
			continue
		}
		if best == "" || v[0] > version[0] || v[0] == version[0] && v[1] > version[1] || v[0] == version[0] && v[1] == version[1] && v[2] > version[2] {
			best, version = tag.Name, v
		}
	}
	return best
}
func (s *RepositorySource) LatestTag(ctx context.Context, plugin string) (string, error) {
	var all []tagReceipt
	for page := 1; ; page++ {
		var tags []tagReceipt
		if err := s.API(ctx, fmt.Sprintf("repos/%s/tags?per_page=100&page=%d", s.Repository.Env["AGENT_GH_REPO"], page), &tags); err != nil {
			return "", err
		}
		all = append(all, tags...)
		if len(tags) < 100 {
			break
		}
	}
	return latestVersionTag(all, plugin+"-v"), nil
}

type commitReceipt struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}
type compareReceipt struct {
	Commits []commitReceipt `json:"commits"`
	Total   int             `json:"total_commits"`
}

func commitText(c commitReceipt) Commit {
	subject, body, _ := strings.Cut(c.Commit.Message, "\n")
	return Commit{Subject: subject, Body: strings.TrimSpace(body)}
}
func (s *RepositorySource) CommitsSince(ctx context.Context, path, tag string) ([]Commit, error) {
	b, err := s.run(ctx, "git", "log", "--format=%H", s.Head, "--", path)
	if err != nil {
		return nil, err
	}
	relevant := map[string]bool{}
	for _, sha := range strings.Fields(string(b)) {
		relevant[sha] = true
	}
	var result []Commit
	count := 0
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("repos/%s/commits?sha=%s&path=%s&per_page=100&page=%d", s.Repository.Env["AGENT_GH_REPO"], url.QueryEscape(s.Head), url.QueryEscape(path), page)
		var commits []commitReceipt
		total := -1
		if tag != "" {
			endpoint = fmt.Sprintf("repos/%s/compare/%s...%s?per_page=100&page=%d", s.Repository.Env["AGENT_GH_REPO"], url.PathEscape(tag), url.PathEscape(s.Head), page)
			var response compareReceipt
			if err := s.API(ctx, endpoint, &response); err != nil {
				return nil, err
			}
			commits, total = response.Commits, response.Total
		} else if err := s.API(ctx, endpoint, &commits); err != nil {
			return nil, err
		}
		for _, c := range commits {
			if relevant[c.SHA] {
				result = append(result, commitText(c))
			}
		}
		count += len(commits)
		if len(commits) < 100 {
			if total >= 0 && count != total {
				return nil, errors.New("incomplete release history")
			}
			break
		}
	}
	return result, nil
}
func (s *RepositorySource) LastMerge(ctx context.Context, path string) (time.Time, error) {
	b, err := s.run(ctx, "git", "log", "--first-parent", "-1", "--format=%cI", s.Head, "--", path)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
}
func (s *RepositorySource) ReadFile(ctx context.Context, path string) ([]byte, error) {
	b, err := s.run(ctx, "git", "ls-tree", "--name-only", s.Head, "--", path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, fs.ErrNotExist
	}
	return s.run(ctx, "git", "show", s.Head+":"+path)
}
