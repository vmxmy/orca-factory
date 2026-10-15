package scheduler

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

// projectTracker is the optional Backend capability that prepares a newly
// discovered repository (its ledger client) before the scheduler reads it.
type projectTracker interface {
	Track(context.Context, config.Repository) error
}

// RefreshProjects re-runs Discover when it is due and replaces the claimable
// set (OF-43). A repository that leaves the eligible set is retired: Tick keeps
// reading it so its in-flight tickets, merges, deployment barriers and
// resolvers finish, but claims none of its new tickets. A failed discovery
// keeps the previous set and is returned so serve can refuse to start on it.
func (s *Scheduler) RefreshProjects(ctx context.Context) error {
	now := s.clock()
	if s.Discover == nil || now.Before(s.nextDiscover) {
		return nil
	}
	s.nextDiscover = now.Add(s.DiscoverEvery)
	result, err := s.Discover(ctx)
	if err != nil {
		s.logger().Warn("project discovery failed; keeping previous projects", "err", err)
		return err
	}
	rejected := map[string]string{}
	for _, r := range result.Rejected {
		rejected[r.Path] = string(r.Reason)
	}
	tracker, _ := s.Backend.(projectTracker)
	eligible := map[string]bool{}
	repositories := slices.Clone(s.Repositories)
	for _, p := range result.Eligible {
		if slices.ContainsFunc(repositories, func(r config.Repository) bool { return r.Path == p.Path }) {
			eligible[p.Path] = true
			continue
		}
		if tracker != nil {
			if err := tracker.Track(ctx, p.Repository); err != nil {
				rejected[p.Path] = err.Error()
				continue
			}
		}
		eligible[p.Path] = true
		repositories = append(repositories, p.Repository)
	}
	retired := map[string]bool{}
	for _, r := range repositories {
		if !eligible[r.Path] {
			retired[r.Path] = true
		}
	}
	s.Repositories, s.retired = repositories, retired
	s.logRejections(rejected)
	paths := make([]string, 0, len(eligible))
	for path := range eligible {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	s.logger().Info("projects discovered", "eligible", paths, "retired", s.retiredPaths())
	return nil
}

// logRejections warns once per repository whenever the rejected set changes.
func (s *Scheduler) logRejections(rejected map[string]string) {
	lines := make([]string, 0, len(rejected))
	for path, reason := range rejected {
		lines = append(lines, path+"\x00"+reason)
	}
	sort.Strings(lines)
	fingerprint := strings.Join(lines, "\n")
	if fingerprint == s.rejections {
		return
	}
	s.rejections = fingerprint
	for _, line := range lines {
		path, reason, _ := strings.Cut(line, "\x00")
		s.logger().Warn("project not taken over", "path", path, "reason", reason)
	}
}

func (s *Scheduler) retiredPaths() []string {
	paths := make([]string, 0, len(s.retired))
	for path := range s.retired {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// dropIdleRetired stops reading a retired repository once the snapshot shows
// it holds no ticket the scheduler or a human still owns.
func (s *Scheduler) dropIdleRetired(tickets []Ticket) {
	if len(s.retired) == 0 {
		return
	}
	busy := map[string]bool{}
	for _, t := range tickets {
		if owned(t) || DeploymentBarrier(t) || t.Owner == machine.OwnerHuman {
			busy[t.Repo] = true
		}
	}
	repositories := make([]config.Repository, 0, len(s.Repositories))
	retired := map[string]bool{}
	for _, r := range s.Repositories {
		if s.retired[r.Path] && !busy[r.Path] {
			continue
		}
		repositories = append(repositories, r)
		if s.retired[r.Path] {
			retired[r.Path] = true
		}
	}
	s.Repositories, s.retired = repositories, retired
}
