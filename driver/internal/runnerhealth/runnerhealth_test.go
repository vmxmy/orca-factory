package runnerhealth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
	failNotes     int
	readErr       error
	// unconfirmedUpdates lands the write, then fails like a lost read-back.
	unconfirmedUpdates int
}

func (f *fakeTickets) Create(_ context.Context, in issues.CreateInput) (issues.Task, error) {
	f.created = append(f.created, in)
	f.state = "open"
	var t issues.Task
	t.ID, t.Identifier, t.NativeState = "uuid-"+strconv.Itoa(len(f.created)), "GH-900", "open"
	return t, nil
}
func (f *fakeTickets) Read(_ context.Context, id string) (issues.Task, error) {
	if err := f.readErr; err != nil {
		f.readErr = nil
		return issues.Task{}, err
	}
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
	if state == "Canceled" {
		f.state = "closed"
	}
	f.labels = slices.DeleteFunc(f.labels, func(label string) bool { return slices.Contains(remove, label) })
	f.labels = append(f.labels, add...)
	if f.unconfirmedUpdates > 0 {
		f.unconfirmedUpdates--
		return task.Issue{}, errors.New("write unconfirmed")
	}
	return task.Issue{}, nil
}
func (f *fakeTickets) CreateComment(_ context.Context, id, body string) (task.Comment, error) {
	if f.failNotes > 0 && strings.Contains(body, "无需施工") {
		f.failNotes--
		return task.Comment{}, errors.New("comment rejected")
	}
	f.comments = append(f.comments, id+": "+body)
	return task.Comment{}, nil
}

func healthyUnits(_ context.Context, spec runx.Spec) ([]byte, error) {
	if spec.Path == "docker" {
		return []byte("1.27.4\n"), nil
	}
	return []byte("ActiveState=active\nNRestarts=0\n"), nil
}

func configureToolchain(t *testing.T, m *Monitor) {
	t.Helper()
	dir := t.TempDir()
	m.GoMod = filepath.Join(dir, "go.mod")
	m.ImageFile = filepath.Join(dir, "image-id.txt")
	for path, content := range map[string]string{
		m.GoMod:     "module example.test/runner\n\ngo 1.25\n\ntoolchain go1.27.4 // pinned for CI\n",
		m.ImageFile: "sha256:runner-image\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeReportsStaleToolchain(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		label string
		err   error
		want  string
	}{
		{name: "outdated", label: "1.26.4\n", want: "镜像 Go 1.26.4"},
		{name: "missing label", want: "镜像 Go 未知"},
		{name: "inspect failed", label: "1.27.4", err: errors.New("image not found"), want: "镜像 Go 未知"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			inspections := 0
			m := &Monitor{StateDir: t.TempDir(), Run: func(ctx context.Context, spec runx.Spec) ([]byte, error) {
				if spec.Path != "docker" {
					return healthyUnits(ctx, spec)
				}
				inspections++
				want := []string{"image", "inspect", "sha256:runner-image", "--format", `{{ index .Config.Labels "orca.go-toolchain" }}`}
				if !slices.Equal(spec.Args, want) {
					t.Fatalf("inspect args = %q, want %q", spec.Args, want)
				}
				return []byte(scenario.label), scenario.err
			}}
			configureToolchain(t, m)
			problems, err := m.probe(context.Background(), time.Now(), &record{Restarts: map[string]int{}})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{scenario.want, "go.mod 要 1.27.4", "CI 每次会重新下载，约 3 分钟", "scripts/local-runner/README.md", "Manual image updates"} {
				if !strings.Contains(problems["toolchain-stale"], want) {
					t.Errorf("toolchain problem = %q, want %q", problems["toolchain-stale"], want)
				}
			}
			if inspections != 1 || len(problems) != 1 {
				t.Errorf("inspections = %d, problems = %v", inspections, problems)
			}
		})
	}
}

func TestProbeToolchainFresh(t *testing.T) {
	inspections := 0
	m := &Monitor{StateDir: t.TempDir(), Run: func(ctx context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path == "docker" {
			inspections++
		}
		return healthyUnits(ctx, spec)
	}}
	configureToolchain(t, m)
	problems, err := m.probe(context.Background(), time.Now(), &record{Restarts: map[string]int{}})
	if err != nil {
		t.Fatal(err)
	}
	if inspections != 1 || len(problems) != 0 {
		t.Fatalf("inspections = %d, problems = %v", inspections, problems)
	}
}

// Real `docker image inspect` stdout for a labelled and an unlabelled runner
// image (driver/CONTRACTS.md); go.mod in configureToolchain pins 1.27.4.
func TestRecordedToolchainLabel(t *testing.T) {
	for sample, want := range map[string]string{
		"docker-image-inspect-go-toolchain-label.txt":         "",
		"docker-image-inspect-go-toolchain-label-missing.txt": "镜像 Go 未知，go.mod 要 1.27.4",
	} {
		t.Run(sample, func(t *testing.T) {
			stdout, err := os.ReadFile(filepath.Join("../../testdata/recorded", sample))
			if err != nil {
				t.Fatal(err)
			}
			m := &Monitor{StateDir: t.TempDir(), Run: func(ctx context.Context, spec runx.Spec) ([]byte, error) {
				if spec.Path == "docker" {
					return stdout, nil
				}
				return healthyUnits(ctx, spec)
			}}
			configureToolchain(t, m)
			problems, err := m.probe(context.Background(), time.Now(), &record{Restarts: map[string]int{}})
			if err != nil {
				t.Fatal(err)
			}
			if got := problems["toolchain-stale"]; (want == "") != (len(problems) == 0) || !strings.HasPrefix(got, want) {
				t.Errorf("problems = %v, want toolchain-stale prefix %q", problems, want)
			}
		})
	}
}

func TestStaleToolchainTicketChecksImageVersion(t *testing.T) {
	tickets := &fakeTickets{}
	m := &Monitor{
		StateDir: t.TempDir(),
		Record:   filepath.Join(t.TempDir(), "runner-health.json"),
		Tickets:  tickets,
		Run: func(ctx context.Context, spec runx.Spec) ([]byte, error) {
			if spec.Path == "docker" {
				return []byte("1.26.4\n"), nil
			}
			return healthyUnits(ctx, spec)
		},
	}
	configureToolchain(t, m)
	start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	if err := m.Poll(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if len(tickets.created) != 0 {
		t.Fatalf("created %d tickets before grace elapsed", len(tickets.created))
	}
	if err := m.Poll(context.Background(), start.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(tickets.created) != 1 {
		t.Fatalf("created %d tickets, want 1", len(tickets.created))
	}
	description := tickets.created[0].Description
	_, acceptance, _ := strings.Cut(description, "\nCHECK: ")
	check, _, _ := strings.Cut(acceptance, "\n")
	_, toolchainCheck, found := strings.Cut(check, "slot-*.json && ")
	if !found {
		t.Fatalf("CHECK lacks the toolchain comparison: %s", check)
	}
	for _, tc := range []struct {
		name, label, goMod string
		dockerFails, pass  bool
	}{
		{name: "fresh", label: "1.27.4", goMod: "toolchain go1.27.4\n", pass: true},
		{name: "stale", label: "1.26.4", goMod: "toolchain go1.27.4\n"},
		{name: "inspect fails and go.mod missing", dockerFails: true},
		{name: "label missing and no toolchain", goMod: "go 1.25\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"bin/docker": "#!/bin/sh\nprintf '%s\\n' \"$LABEL\"\n[ -z \"$FAIL\" ]\n",
				".local/lib/orca-go-isolated-runner/image-id.txt": "sha256:runner-image\n",
			} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.goMod != "" {
				if err := os.MkdirAll(filepath.Join(dir, "driver"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "driver", "go.mod"), []byte(tc.goMod), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fail := ""
			if tc.dockerFails {
				fail = "1"
			}
			cmd := exec.Command("bash", "-c", toolchainCheck)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "LABEL="+tc.label, "FAIL="+fail)
			if err := cmd.Run(); (err == nil) != tc.pass {
				t.Errorf("toolchain CHECK error = %v, want pass %v: %s", err, tc.pass, toolchainCheck)
			}
		})
	}
	_, actions, _ := strings.Cut(description, "## 要做\n")
	actions, _, _ = strings.Cut(actions, "\n## 验收")
	if !strings.Contains(actions, "Manual image updates") {
		t.Errorf("ticket actions lack Manual image updates: %s", actions)
	}
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
			configureToolchain(t, m)
			for _, minutes := range []int{15, 20, 26, 31} {
				if err := m.Poll(context.Background(), start.Add(time.Duration(minutes)*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if len(tickets.created) != 1 {
				t.Fatalf("created %d tickets, want 1", len(tickets.created))
			}
			in := tickets.created[0]
			if strings.Contains(in.Description, "orca.go-toolchain") {
				t.Errorf("slot-only ticket includes toolchain check:\n%s", in.Description)
			}
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
	configureToolchain(t, m)
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
	run := func(ctx context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path == "docker" {
			return healthyUnits(ctx, spec)
		}
		restarts += 10
		return []byte("ActiveState=activating\nNRestarts=" + strconv.Itoa(restarts) + "\n"), nil
	}
	tickets := &fakeTickets{state: "open"}
	m := &Monitor{StateDir: t.TempDir(), Record: filepath.Join(t.TempDir(), "runner-health.json"), Tickets: tickets, Run: run}
	configureToolchain(t, m)
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

// outage opens the stuck-registration ticket, then makes every slot healthy.
func outage(t *testing.T, tickets *fakeTickets) (*Monitor, time.Time) {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	writeLease(t, dir, "registering", start)
	m := &Monitor{StateDir: dir, Record: filepath.Join(t.TempDir(), "runner-health.json"), Tickets: tickets, Run: healthyUnits}
	configureToolchain(t, m)
	for _, minutes := range []int{15, 20, 26, 31} {
		if err := m.Poll(context.Background(), start.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	writeLease(t, dir, "registered", start.Add(32*time.Minute))
	return m, start.Add(35 * time.Minute)
}

func readRecord(t *testing.T, m *Monitor) record {
	t.Helper()
	b, err := os.ReadFile(m.Record)
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestWithdrawalNoteRetriedAfterFailure(t *testing.T) {
	tickets := &fakeTickets{state: "open", workflowState: "Todo", labels: []string{"ready-for-agent"}, failNotes: 1}
	m, now := outage(t, tickets)
	if err := m.Poll(context.Background(), now); err == nil {
		t.Fatal("failed withdrawal note must surface")
	}
	if rec := readRecord(t, m); tickets.workflowState != "Canceled" || tickets.updates != 1 || len(tickets.comments) != 1 || rec.RecoveryStep != stepWithdrawn || rec.Recovered {
		t.Fatalf("after failed note: state=%s updates=%d comments=%q record=%+v", tickets.workflowState, tickets.updates, tickets.comments, rec)
	}
	for _, minutes := range []int{5, 10} {
		if err := m.Poll(context.Background(), now.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if rec := readRecord(t, m); len(tickets.comments) != 2 || !strings.Contains(tickets.comments[1], "故障已自行恢复，无需施工") || tickets.updates != 1 || tickets.recoveryReads != 1 || !rec.Recovered || rec.RecoveryStep != "" {
		t.Fatalf("retry must add only the note: updates=%d reads=%d comments=%q record=%+v", tickets.updates, tickets.recoveryReads, tickets.comments, rec)
	}
}

func TestWithdrawalNoteAfterUnconfirmedCancel(t *testing.T) {
	tickets := &fakeTickets{state: "open", workflowState: "Todo", labels: []string{"ready-for-agent"}, unconfirmedUpdates: 1}
	m, now := outage(t, tickets)
	if err := m.Poll(context.Background(), now); err == nil {
		t.Fatal("unconfirmed withdrawal must surface")
	}
	if err := m.Poll(context.Background(), now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if rec := readRecord(t, m); len(tickets.comments) != 2 || !strings.Contains(tickets.comments[1], "故障已自行恢复，无需施工") || tickets.updates != 1 || !rec.Recovered {
		t.Fatalf("landed withdrawal lost its note: updates=%d comments=%q record=%+v", tickets.updates, tickets.comments, rec)
	}
}

func TestWithdrawalNoteSurvivesReturningOutage(t *testing.T) {
	tickets := &fakeTickets{state: "open", workflowState: "Todo", labels: []string{"ready-for-agent"}, failNotes: 1}
	m, now := outage(t, tickets)
	if err := m.Poll(context.Background(), now); err == nil {
		t.Fatal("failed withdrawal note must surface")
	}
	writeLease(t, m.StateDir, "registering", now)
	for _, minutes := range []int{15, 20, 26} {
		if err := m.Poll(context.Background(), now.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	notes := slices.DeleteFunc(slices.Clone(tickets.comments), func(c string) bool { return !strings.Contains(c, "无需施工") })
	if len(tickets.created) != 2 || len(notes) != 1 || !strings.HasPrefix(notes[0], "uuid-1: ") {
		t.Fatalf("returning outage dropped the owed note: created=%d comments=%q", len(tickets.created), tickets.comments)
	}
}

func TestOwedNoteSavedBeforeTicketRead(t *testing.T) {
	tickets := &fakeTickets{state: "open", workflowState: "Todo", labels: []string{"ready-for-agent"}, failNotes: 2}
	m, now := outage(t, tickets)
	writeLease(t, m.StateDir, "registering", now)
	for _, minutes := range []int{0, 15} {
		if err := m.Poll(context.Background(), now.Add(time.Duration(minutes)*time.Minute)); err == nil {
			t.Fatal("failed withdrawal note must surface")
		}
	}
	tickets.readErr = errors.New("tracker unavailable")
	if err := m.Poll(context.Background(), now.Add(26*time.Minute)); err == nil {
		t.Fatal("failed ticket read must surface")
	}
	if err := m.Poll(context.Background(), now.Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	notes := slices.DeleteFunc(slices.Clone(tickets.comments), func(c string) bool { return !strings.Contains(c, "无需施工") })
	if len(notes) != 1 || len(tickets.created) != 2 {
		t.Fatalf("note must land once before the read fails: created=%d comments=%q", len(tickets.created), tickets.comments)
	}
}

func TestRecordedOwnershipCommentBlocksWithdrawal(t *testing.T) {
	for _, name := range []string{"linear-comment-ownership-unregistered.json", "linear-comments-executor-handoff.json"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "recorded", name))
			if err != nil {
				t.Fatal(err)
			}
			var resp struct {
				Data struct {
					Issue struct {
						Comments struct{ Nodes []task.Comment } `json:"comments"`
					} `json:"issue"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatal(err)
			}
			recorded := resp.Data.Issue.Comments.Nodes
			unowned := slices.DeleteFunc(slices.Clone(recorded), func(c task.Comment) bool { return strings.Contains(c.Body, "归属: ") })
			for _, scenario := range []struct {
				history    []task.Comment
				wantCancel bool
			}{{history: recorded}, {history: unowned, wantCancel: true}} {
				tickets := &fakeTickets{state: "open", workflowState: "Todo", labels: []string{"ready-for-agent"}, history: scenario.history}
				m, now := outage(t, tickets)
				if err := m.Poll(context.Background(), now); err != nil {
					t.Fatal(err)
				}
				canceled := tickets.workflowState == "Canceled" && tickets.updates == 1 && len(tickets.comments) == 2
				untouched := tickets.workflowState == "Todo" && tickets.updates == 0 && len(tickets.comments) == 1 && slices.Equal(tickets.labels, []string{"ready-for-agent"})
				if (scenario.wantCancel && !canceled) || (!scenario.wantCancel && !untouched) {
					t.Fatalf("history %d comments, want cancel %v: state=%s labels=%v updates=%d comments=%q", len(scenario.history), scenario.wantCancel, tickets.workflowState, tickets.labels, tickets.updates, tickets.comments)
				}
			}
		})
	}
}
