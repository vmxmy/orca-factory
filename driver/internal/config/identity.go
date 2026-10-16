package config

import "sort"

// LedgerIdentityKeys select a repository's ledger backend and target project.
// A repository's bound Linear/GitHub/Feishu client keeps using the value it
// started with, so a hot reload of agent-graph.env (OF-45) must never swap
// one of these underneath a live client; the repository keeps its old
// configuration until serve restarts.
var LedgerIdentityKeys = []string{
	"AGENT_ISSUE_BACKEND",
	"AGENT_FEISHU_BASE",
	"AGENT_FEISHU_PROJECT",
	"AGENT_LINEAR_TEAM",
	"AGENT_GH_REPO",
}

// ChangedEnvKeys returns the sorted Env keys whose value differs between old
// and new, including a key present on only one side. Callers log only these
// names; the values themselves never leave this function.
func ChangedEnvKeys(old, updated map[string]string) []string {
	seen := map[string]struct{}{}
	for k := range old {
		seen[k] = struct{}{}
	}
	for k := range updated {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		if old[k] != updated[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
