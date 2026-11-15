package scheduler

import (
	"slices"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// dependencyDiagnostics observes the complete snapshot without adding roots to admission.
func dependencyDiagnostics(snapshot []task.Issue, comments map[string][]task.Comment, tickets []Ticket) {
	byID := make(map[string]task.Issue, len(snapshot))
	for _, issue := range snapshot {
		byID[issue.ID] = issue
	}
	affected := map[string][]string{}
	for index := range tickets {
		ticket := &tickets[index]
		if !ticket.Blocked {
			continue
		}
		visited := map[string]bool{}
		active := map[string]bool{ticket.ID: true}
		visits, limited := 0, false
		var visit func(string, string, []string)
		visit = func(id, identifier string, chain []string) {
			if visited[id] && !active[id] {
				return
			}
			if limited {
				return
			}
			visits++
			issue, exists := byID[id]
			dep := status.Dependency{Identifier: identifier, Labels: []string{}, Owner: string(machine.OwnerNone), Status: "unknown", Chain: append(slices.Clone(chain), identifier)}
			owner := Ticket{}
			var ownershipErr error
			if exists {
				dep, owner, ownershipErr = dependencyEvidence(issue, comments[id], dep.Chain)
			}
			if len(chain) >= 64 || visits > 1000 {
				limited = true
				dep.Reason = "dependency traversal limit reached"
			} else if !exists {
				dep.Reason = "dependency missing from snapshot"
			} else if active[id] {
				dep.Status, dep.Reason = "cycle", "dependency cycle"
			} else {
				visited[id], active[id] = true, true
				defer delete(active, id)
				switch {
				case ownershipErr != nil:
					dep.Candidate, dep.Reason = false, "ownership evidence invalid"
				case issue.Labels.PageInfo.HasNextPage || issue.Relations.PageInfo.HasNextPage || issue.InverseRelations.PageInfo.HasNextPage:
					dep.Candidate, dep.Reason = false, "dependency evidence incomplete"
				default:
					nested := false
					for _, relation := range issue.InverseRelations.Nodes {
						if relation.Type != "blocks" {
							continue
						}
						switch relation.Issue.State.Type {
						case "completed", "canceled", "duplicate":
							continue
						}
						nested = true
						visit(relation.Issue.ID, relation.Issue.Identifier, dep.Chain)
					}
					if nested {
						return
					}
					switch {
					case executorUnregistered(owner):
						dep.Status, dep.Reason = "unknown", status.ExecutorStartUnconfirmed
					case owner.Owner != "" && owner.Owner != machine.OwnerNone:
						dep.Status, dep.Reason = "waiting", "owned by "+dep.Owner
					case issue.Held:
						dep.Status, dep.Reason = "stalled", "import held"
					case issue.State.Name == "":
						dep.Candidate, dep.Reason = false, "unknown state"
					case issue.State.Name != "Todo" && issue.State.Name != "Backlog":
						dep.Status, dep.Reason = "stalled", "state "+issue.State.Name+" is not Todo/Backlog"
					case !slices.Contains(dep.Labels, "ready-for-agent"):
						dep.Status, dep.Reason = "stalled", "missing ready-for-agent"
					case !dep.Candidate:
						dep.Status, dep.Reason = "stalled", "labels exclude automatic candidacy"
					default:
						dep.Status, dep.Reason = "waiting", "automatic candidate; admission checks still apply"
					}
				}
			}
			ticket.Dependencies = append(ticket.Dependencies, dep)
			key := ticket.Repo + "\x00" + dep.Identifier + "\x00" + dep.Reason
			visited[id] = true
			if !slices.Contains(affected[key], ticket.Identifier) {
				affected[key] = append(affected[key], ticket.Identifier)
			}
		}
		issue := byID[ticket.ID]
		if issue.InverseRelations.PageInfo.HasNextPage {
			dep, _, _ := dependencyEvidence(issue, comments[issue.ID], []string{ticket.Identifier})
			dep.Reason = "dependency evidence incomplete"
			ticket.Dependencies = append(ticket.Dependencies, dep)
		}
		for _, relation := range issue.InverseRelations.Nodes {
			if relation.Type != "blocks" {
				continue
			}
			switch relation.Issue.State.Type {
			case "completed", "canceled", "duplicate":
				continue
			}
			visit(relation.Issue.ID, relation.Issue.Identifier, []string{ticket.Identifier})
		}
	}
	for index := range tickets {
		ticket := &tickets[index]
		for index := range ticket.Dependencies {
			dep := &ticket.Dependencies[index]
			dep.Affected = slices.Clone(affected[ticket.Repo+"\x00"+dep.Identifier+"\x00"+dep.Reason])
			if len(dep.Affected) == 0 {
				dep.Affected = []string{ticket.Identifier}
			}
			slices.Sort(dep.Affected)
		}
	}
}

func dependencyReason(dependencies []status.Dependency) string {
	reasons := make([]string, 0, len(dependencies))
	for _, dep := range dependencies {
		candidate := "no"
		if dep.Candidate {
			candidate = "yes"
		}
		reasons = append(reasons, strings.Join(dep.Chain, " -> ")+": "+dep.Title+" [state="+dep.State+" labels="+strings.Join(dep.Labels, ",")+" owner="+dep.Owner+" candidate="+candidate+"] "+dep.Reason+"; affected="+strings.Join(dep.Affected, ","))
	}
	return strings.Join(reasons, "; ")
}

func dependencyEvidence(issue task.Issue, comments []task.Comment, chain []string) (status.Dependency, Ticket, error) {
	dep := status.Dependency{Identifier: issue.Identifier, Title: issue.Title, State: issue.State.Name, Labels: []string{}, Owner: string(machine.OwnerNone), Status: "unknown", Chain: chain}
	for _, label := range issue.Labels.Nodes {
		dep.Labels = append(dep.Labels, label.Name)
	}
	slices.Sort(dep.Labels)
	owner := Ticket{}
	err := readOwnership(&owner, comments)
	if owner.Owner != "" {
		dep.Owner = string(owner.Owner)
	}
	dep.Candidate, _, _ = issueEligibility(issue)
	return dep, owner, err
}
