// Package release plans plugin releases and hands their PRs to the shared merge
// lane. Hosts supply repository snapshots, PR writes, and publication adapters.
package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

type Commit struct{ Subject, Body string }

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var subjectPattern = regexp.MustCompile(`^([a-zA-Z]+)(\([^()\r\n]+\))?(!)?:`)
var pluginPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Bump accepts a stable major.minor.patch version. Breaking changes dominate
// features; all other changes, including an empty list, increment patch.
func Bump(current string, commits []Commit) (string, error) {
	parts := versionPattern.FindStringSubmatch(current)
	if parts == nil {
		return "", errors.New("invalid stable version")
	}
	var v [3]uint64
	for i := range v {
		n, err := strconv.ParseUint(parts[i+1], 10, 64)
		if err != nil {
			return "", errors.New("version component out of range")
		}
		v[i] = n
	}
	level := 2
	for _, c := range commits {
		m := subjectPattern.FindStringSubmatch(c.Subject)
		if strings.Contains(c.Body, "BREAKING CHANGE:") || (m != nil && m[3] == "!") {
			if v[0] == 0 {
				level = 1
			} else {
				level = 0
			}
			break
		}
		if m != nil && m[1] == "feat" {
			level = 1
		}
	}
	if v[level] == ^uint64(0) {
		return "", errors.New("version overflow")
	}
	v[level]++
	for i := level + 1; i < len(v); i++ {
		v[i] = 0
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), nil
}

type Plan struct {
	Base                     string // immutable main snapshot used to generate these bytes
	Plugin, Version, Section string
	Files                    map[string][]byte
}

// Generate changes exactly two repository-relative files. Existing JSON bytes
// outside the top-level version value are preserved, including whitespace.
// A nil changelog represents an absent file. New sections append to existing text.
func Generate(plugin string, manifest, changelog []byte, commits []Commit) (Plan, error) {
	if !pluginPattern.MatchString(plugin) {
		return Plan{}, errors.New("invalid plugin name")
	}
	if !json.Valid(manifest) {
		return Plan{}, errors.New("invalid plugin manifest")
	}
	d := json.NewDecoder(bytes.NewReader(manifest))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return Plan{}, errors.New("plugin manifest must be an object")
	}
	start, end := -1, -1
	var current string
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return Plan{}, errors.New("invalid manifest key")
		}
		offset := int(d.InputOffset())
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return Plan{}, errors.New("invalid manifest value")
		}
		if key == "version" {
			if start >= 0 {
				return Plan{}, errors.New("duplicate version field")
			}
			if err := json.Unmarshal(value, &current); err != nil {
				return Plan{}, errors.New("version must be a string")
			}
			for offset < len(manifest) && (manifest[offset] == ':' || strings.ContainsRune(" \t\r\n", rune(manifest[offset]))) {
				offset++
			}
			start, end = offset, int(d.InputOffset())
		}
	}
	if start < 0 {
		return Plan{}, errors.New("missing version field")
	}
	next, err := Bump(current, commits)
	if err != nil {
		return Plan{}, err
	}
	encoded, _ := json.Marshal(next) // A string always has a JSON encoding.
	updated := append([]byte(nil), manifest[:start]...)
	updated = append(updated, encoded...)
	updated = append(updated, manifest[end:]...)
	section := "## " + next + "\n\n"
	for _, c := range commits {
		if strings.ContainsAny(c.Subject, "\r\n") {
			return Plan{}, errors.New("commit subject must be one line")
		}
		section += "- " + c.Subject + "\n"
	}
	log := append([]byte(nil), changelog...)
	if len(log) > 0 {
		if log[len(log)-1] != '\n' {
			log = append(log, '\n')
		}
		log = append(log, '\n')
	}
	log = append(log, section...)
	root := "mods/" + plugin
	return Plan{Plugin: plugin, Version: next, Section: section, Files: map[string][]byte{root + "/.claude-plugin/plugin.json": updated, root + "/CHANGELOG.md": log}}, nil
}

// Source is scoped to one repository and one consistent snapshot. Plugins lists
// immediate plugin directories under mods. LatestTag returns the last published
// <plugin>-v<version> tag or empty when never released. CommitsSince returns only
// commits touching path after tag (all relevant history when tag is empty).
// LastMerge returns the latest relevant merge time, not the commit author date;
// for never-merged history it returns the latest integration time. ReadFile uses
// fs.ErrNotExist for absent files. Env reads repository environment configuration.
// Implementations own bounded external I/O and must not invoke real services in tests.
type Source interface {
	Plugins(context.Context) ([]string, error)
	Env(context.Context, string) (string, error)
	LatestTag(context.Context, string) (string, error)
	CommitsSince(context.Context, string, string) ([]Commit, error)
	LastMerge(context.Context, string) (time.Time, error)
	ReadFile(context.Context, string) ([]byte, error)
}

// Scan plans releases only after strictly more than the quiet interval has
// elapsed. The injected clock is sampled once; invalid configuration fails closed.
func Scan(ctx context.Context, src Source, now func() time.Time) ([]Plan, error) {
	if src == nil || now == nil {
		return nil, errors.New("release scan dependencies required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := src.Env(ctx, "AGENT_RELEASE_QUIET_MINUTES")
	if err != nil {
		return nil, errors.New("release configuration unavailable")
	}
	minutes := int64(30)
	if raw != "" {
		minutes, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || minutes < 0 || minutes > int64((1<<63-1)/time.Minute) {
			return nil, errors.New("invalid release quiet minutes")
		}
	}
	quiet := time.Duration(minutes) * time.Minute
	instant := now()
	plugins, err := src.Plugins(ctx)
	if err != nil {
		return nil, errors.New("plugin list unavailable")
	}
	var plans []Plan
	for _, plugin := range plugins {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !pluginPattern.MatchString(plugin) {
			return nil, errors.New("invalid plugin name")
		}
		path := "mods/" + plugin
		tag, err := src.LatestTag(ctx, plugin)
		if err != nil {
			return nil, errors.New("release tag unavailable")
		}
		if tag != "" && (!strings.HasPrefix(tag, plugin+"-v") || !versionPattern.MatchString(strings.TrimPrefix(tag, plugin+"-v"))) {
			return nil, errors.New("invalid release tag")
		}
		commits, err := src.CommitsSince(ctx, path, tag)
		if err != nil {
			return nil, errors.New("plugin history unavailable")
		}
		if len(commits) == 0 {
			continue
		}
		last, err := src.LastMerge(ctx, path)
		if err != nil {
			return nil, errors.New("plugin merge time unavailable")
		}
		if last.IsZero() {
			return nil, errors.New("plugin merge time missing")
		}
		if instant.Sub(last) <= quiet {
			continue
		}
		manifest, err := src.ReadFile(ctx, path+"/.claude-plugin/plugin.json")
		if err != nil {
			return nil, errors.New("plugin manifest unavailable")
		}
		log, err := src.ReadFile(ctx, path+"/CHANGELOG.md")
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, errors.New("plugin changelog unavailable")
			}
			log = nil
		}
		plan, err := Generate(plugin, manifest, log, commits)
		if err != nil {
			return nil, err
		}
		if completer, ok := src.(interface {
			CompletePlan(context.Context, *Plan) error
		}); ok {
			if err := completer.CompletePlan(ctx, &plan); err != nil {
				return nil, err
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// PRWriter writes exactly Plan.Files to a release branch and opens (or recovers)
// its PR, returning its number. Adapters own branch creation and bounded I/O.
type PRWriter interface {
	CreateReleasePR(context.Context, merge.Request, Plan) (int, error)
}
type Queue interface{ Enqueue(merge.Request) error }
type Pending struct {
	Request merge.Request
	Plan    Plan
}

// Submit returns Pending even when enqueue fails after PR creation; the host can
// retry Queue.Enqueue without creating another PR. Hosts persist Pending until
// the matching merge result arrives and publication succeeds.
func Submit(ctx context.Context, writer PRWriter, queue Queue, request merge.Request, plan Plan) (Pending, error) {
	if writer == nil || queue == nil {
		return Pending{}, errors.New("release submission dependencies required")
	}
	if err := ctx.Err(); err != nil {
		return Pending{}, err
	}
	if _, _, ok := repository(request.Repository); !ok {
		return Pending{}, errors.New("invalid repository")
	}
	number, err := writer.CreateReleasePR(ctx, request, plan)
	if err != nil {
		return Pending{}, errors.New("release PR creation failed")
	}
	if number <= 0 {
		return Pending{}, errors.New("release PR number missing")
	}
	request.Number = number
	pending := Pending{Request: request, Plan: plan}
	if err := queue.Enqueue(request); err != nil {
		return pending, errors.New("release PR enqueue failed")
	}
	return pending, nil
}

// Publisher is satisfied by *github.Client. Hosts must persist completion and
// provide recovery for an uncertain/partial write before replaying publication.
type Publisher interface {
	CreateTag(context.Context, string, string, string, string) error
	CreateRelease(context.Context, string, string, github.ReleaseRequest) (github.Release, error)
}

// Publish must receive the authoritative merge-channel result. Waiting results
// do nothing; only a matching Merged result may create a tag and release.
func Publish(ctx context.Context, publisher Publisher, pending Pending, result merge.Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Status != merge.Merged {
		return nil
	}
	if publisher == nil {
		return errors.New("release publisher required")
	}
	if pending.Request.Repository != result.Request.Repository || pending.Request.Number <= 0 || pending.Request.Number != result.Request.Number || result.MergeSHA == "" {
		return errors.New("release merge result mismatch")
	}
	owner, repo, ok := repository(pending.Request.Repository)
	if !ok {
		return errors.New("invalid repository")
	}
	if !pluginPattern.MatchString(pending.Plan.Plugin) || !versionPattern.MatchString(pending.Plan.Version) {
		return errors.New("invalid release identity")
	}
	tag := pending.Plan.Plugin + "-v" + pending.Plan.Version
	if err := publisher.CreateTag(ctx, owner, repo, tag, result.MergeSHA); err != nil {
		return errors.New("release tag creation failed")
	}
	_, err := publisher.CreateRelease(ctx, owner, repo, github.ReleaseRequest{TagName: tag, Name: tag, Body: pending.Plan.Section})
	if err != nil {
		return errors.New("release creation failed")
	}
	return nil
}
func repository(value string) (string, string, bool) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
