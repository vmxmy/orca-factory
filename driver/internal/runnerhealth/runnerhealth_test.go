package runnerhealth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type fakeTickets struct {
	created       []issues.CreateInput
	comments      []string
	state         string
	workflowState string
	labels        []string
	history       []task.Comment
	updates       int
	recoveryReads int
}

func (f *fakeTickets) Create(_ context.Context, in issues.CreateInput) (issues.Task, error) {
	f.created = append(f.created, in)
	var t issues.Task
	t.ID, t.Identifier, t.NativeState = "uuid-1", "GH-900", "open"
	return t, nil
}
func (f *fakeTickets) Read(_ context.Context, id string) (issues.Task, error) {
	if len(f.comments) > 0 {
		f.recoveryReads++
	}
	var t issues.Task
	t.ID, t.NativeState = id, f.state
	t.State.Name = f.workflowState
	for _, label := range f.labels {
		t.Labels.Nodes = append(t.Labels.Nodes, task.NamedID{Name: label})
	}
	return t, nil
}
func (f *fakeTickets) ListComments(context.Context, string) ([]task.Comment, error) {
	return f.history, nil
}
func (f *fakeTickets) UpdateIssue(_ context.Context, _ string, state string, add, remove []string) (task.Issue, error) {
	f.updates++
	f.workflowState = state
	f.labels = slices.DeleteFunc(f.labels, func(label string) bool { return slices.Contains(remove, label) })
	f.labels = append(f.labels, add...)
	return task.Issue{}, nil
}
func (f *fakeTickets) CreateComment(_ context.Context, id, body string) (task.Comment, error) {
	f.comments = append(f.comments, id+": "+body)
	return task.Comment{}, nil
}

func healthyUnits(context.Context, runx.Spec) ([]byte, error) {
	return []byte("ActiveState=active\nNRestarts=0\n"), nil
}

func writeLease(t *testing.T, dir, phase string, modified time.Time) {
	t.Helper()
	path := filepath.Join(dir, "slot-3.json")
	b, _ := json.Marshal(map[string]any{"name": "orca-go-x-3-abc", "active": true, "phase": phase})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// The 2026-10-12 outage: a lost registration POST left slots stuck in
// "registering" for 18 hours with no one notified.
func TestStuckRegistrationOpensOneAgentTicketThenCommentsRecovery(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		state      string
		labels     []string
		history    []task.Comment
		wantCancel bool
	}{
		{name: "unclaimed", state: "Todo", labels: []string{"ready-for-agent", "runner"}, wantCancel: true},
		{name: "auto-impl", state: "Todo", labels: []string{"ready-for-agent", "auto-impl"}},
		{name: "ownership", state: "Todo", labels: []string{"ready-for-agent"}, history: []task.Comment{{Body: "阶段: gate\n归属: 执行器 term=worker"}}},
		{name: "in progress", state: "In Progress", labels: []string{"ready-for-agent"}},
		{name: "not ready", state: "Todo", labels: []string{"runner"}},
		{name: "completed", state: "Done", labels: []string{"ready-for-agent"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
			writeLease(t, dir, "registering", start)
			tickets := &fakeTickets{state: "open", workflowState: scenario.state, labels: slices.Clone(scenario.labels), history: scenario.history}
			m := &Monitor{StateDir: dir, Record: filepath.Join(t.TempDir(), "runner-health.json"), Tickets: tickets, Run: healthyUnits}
			for _, minutes := range []int{15, 20, 26, 31} {
				if err := m.Poll(context.Background(), start.Add(time.Duration(minutes)*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if len(tickets.created) != 1 {
				t.Fatalf("created %d tickets, want 1", len(tickets.created))
			}
			in := tickets.created[0]
			if strings.Join(in.Labels, ",") != "ready-for-agent" || in.Blockers == nil || in.Related == nil {
				t.Fatalf("ticket must be agent-claimable and managed: %+v", in)
			}
			for _, want := range []string{"orca-go-x-3-abc", "CHECK:", "## SCOPE", "DEPENDENCIES: {\"blockers\":[],\"related\":[]}"} {
				if !strings.Contains(in.Description, want) {
					t.Fatalf("description lacks %q:\n%s", want, in.Description)
				}
			}
			if !strings.Contains(in.Description, "## SCOPE\nscripts/local-runner/* scripts/local-runner/maintenance/*\n") {
				t.Errorf("description lacks runner and maintenance file scope:\n%s", in.Description)
			}
			writeLease(t, dir, "registered", start.Add(32*time.Minute))
			for _, minutes := range []int{35, 40} {
				if err := m.Poll(context.Background(), start.Add(time.Duration(minutes)*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			wantComments := 1
			if scenario.wantCancel {
				wantComments = 2
			}
			if len(tickets.comments) != wantComments || !strings.HasPrefix(tickets.comments[0], "uuid-1: ") {
				t.Fatalf("recovery comments = %q", tickets.comments)
			}
			if tickets.recoveryReads != 1 {
				t.Errorf("ticket reads after recovery comment = %d, want 1", tickets.recoveryReads)
			}
			if scenario.wantCancel {
				if tickets.workflowState != "Canceled" || !slices.Equal(tickets.labels, []string{"runner"}) || tickets.updates != 1 {
					t.Errorf("unclaimed recovery: state=%s labels=%v updates=%d", tickets.workflowState, tickets.labels, tickets.updates)
				}
				if !strings.Contains(tickets.comments[1], "故障已自行恢复，无需施工") {
					t.Errorf("recovery comment lacks withdrawal reason: %s", tickets.comments[1])
				}
			} else if tickets.workflowState != scenario.state || !slices.Equal(tickets.labels, scenario.labels) || tickets.updates != 0 {
				t.Errorf("recovery changed an ineligible ticket: state=%s labels=%v updates=%d", tickets.workflowState, tickets.labels, tickets.updates)
			}
		})
	}
}

func TestBriefProblemDoesNotAlert(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	writeLease(t, dir, "registering", start)
	tickets := &fakeTickets{state: "open"}
	m := &Monitor{StateDir: dir, Record: filepath.Join(t.TempDir(), "runner-health.json"), Tickets: tickets, Run: healthyUnits}
	if err := m.Poll(context.Background(), start.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	writeLease(t, dir, "registered", start.Add(12*time.Minute))
	if err := m.Poll(context.Background(), start.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(tickets.created) != 0 {
		t.Fatalf("transient problem opened %d tickets", len(tickets.created))
	}
}

func TestRestartLoopIsAProblem(t *testing.T) {
	restarts := 0
	run := func(context.Context, runx.Spec) ([]byte, error) {
		restarts += 10
		return []byte("ActiveState=activating\nNRestarts=" + strconv.Itoa(restarts) + "\n"), nil
	}
	tickets := &fakeTickets{state: "open"}
	m := &Monitor{StateDir: t.TempDir(), Record: filepath.Join(t.TempDir(), "runner-health.json"), Tickets: tickets, Run: run}
	start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for _, minutes := range []int{0, 5, 10, 15} {
		if err := m.Poll(context.Background(), start.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(tickets.created) != 1 || !strings.Contains(tickets.created[0].Description, "NRestarts") {
		t.Fatalf("restart loop tickets = %+v", tickets.created)
	}
}
