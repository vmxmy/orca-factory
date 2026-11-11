package mergechannel

import (
	"errors"
	"path"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

func HandoffBranch(h *merge.Handoff) string { return "agent/" + h.Identifier + "/" + h.Unit }

// PRMatchesHandoff is the PR identity the merge lane accepts for a handoff.
func PRMatchesHandoff(p github.PullRequest, h *merge.Handoff) bool {
	return p.Number == h.PR && p.Head.SHA == h.Head && p.Head.Ref == HandoffBranch(h) && p.Head.Repo.FullName == h.Repository && p.Base.Ref == "main" && p.Base.Repo.FullName == h.Repository
}

func Policy(env map[string]string, lane int, approved bool, files []string) error {
	if lane == 2 && env["AGENT_LANE2_AUTOMERGE"] != "1" && !approved {
		return merge.ErrReviewRequired
	}
	for _, key := range []string{"AGENT_LANE3_GLOBS", "AGENT_LANE2_MANUAL_GLOBS", "AGENT_LANE2_GLOBS"} {
		for _, pattern := range strings.Fields(env[key]) {
			if _, err := path.Match(pattern, ""); err != nil {
				return errors.New("invalid merge policy glob")
			}
			for _, file := range files {
				if !task.ScopeMatch(pattern, file) {
					continue
				}
				switch key {
				case "AGENT_LANE3_GLOBS":
					return merge.ErrHumanRequired
				case "AGENT_LANE2_MANUAL_GLOBS":
					if approved {
						continue
					}
					return merge.ErrReviewRequired
				default: // AGENT_LANE2_GLOBS
					if lane != 2 {
						return merge.ErrReviewRequired
					}
				}
			}
		}
	}
	return nil
}
