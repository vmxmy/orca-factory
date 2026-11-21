package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// Contract tests read payloads recorded from the real systems (testdata/recorded).
// Hand-written fakes encode our assumptions; recorded payloads encode the systems.
func recorded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordedOwnershipUnregistered(t *testing.T) {
	var resp struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []task.Comment } `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorded(t, "linear-comment-ownership-unregistered.json"), &resp); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, resp.Data.Issue.Comments.Nodes); err != nil {
		t.Fatal(err)
	}
	if !unregistered(ticket) {
		t.Fatalf("scheduler claim with term=- must read as unregistered, got terminal %q", ticket.Terminal)
	}
}

func TestRecordedTerminalList(t *testing.T) {
	raw := recorded(t, "orca-terminal-list.json")
	path := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'RECORDED'\n"+string(raw)+"\nRECORDED\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := &orca.Client{Path: path}
	terminals, err := client.TerminalList(context.Background())
	if err != nil || len(terminals) != 2 {
		t.Fatalf("terminals=%+v err=%v", terminals, err)
	}
	for _, terminal := range terminals {
		if terminal.AgentIdentity != "claude" {
			t.Fatalf("recorded agent identity=%q want claude", terminal.AgentIdentity)
		}
	}
	r := &Runtime{OrcaPath: "/usr/bin/orca", Run: func(context.Context, runx.Spec) ([]byte, error) { return raw, nil }}
	if absent, err := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_640d9460-b9fd-46a0-bedb-09763c8c20ea"}); err != nil || absent {
		t.Fatalf("listed terminal: absent=%v err=%v", absent, err)
	}
	if absent, err := r.TerminalAbsent(context.Background(), Ticket{Terminal: "term_gone"}); err != nil || !absent {
		t.Fatalf("missing terminal: absent=%v err=%v", absent, err)
	}
}

func TestRecordedExecutorHandoff(t *testing.T) {
	var resp struct {
		Data struct {
			Issue struct {
				Comments struct{ Nodes []task.Comment } `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorded(t, "linear-comments-executor-handoff.json"), &resp); err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{}
	if err := readOwnership(&ticket, resp.Data.Issue.Comments.Nodes); err != nil {
		t.Fatal(err)
	}
	if ticket.Owner != machine.OwnerHuman {
		t.Fatalf("a 转人 comment hands the ticket to a human, got owner %q", ticket.Owner)
	}
}

func TestRecordedSchedulerBlockerIdentifiers(t *testing.T) {
	var response struct{ Data struct{ Issue task.Issue } }
	if err := json.Unmarshal(recorded(t, "linear-inverse-relations.json"), &response); err != nil {
		t.Fatal(err)
	}
	issue := response.Data.Issue
	issue.Project.ID = "project"
	issue.State.Name = "Todo"
	issue.Labels.Nodes = []task.NamedID{{Name: "ready-for-agent"}}
	client := &fakeIssues{issues: []task.Issue{issue}}
	repo := config.Repository{Path: "repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "project", "AGENT_SCOPE_AVOID": "0"}}
	runtime := &Runtime{Clients: map[string]issues.Store{"repo": client}}
	tickets, err := runtime.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || !tickets[0].Blocked || tickets[0].BlockersIncomplete || !reflect.DeepEqual(tickets[0].Blockers, []string{"ZII-893"}) {
		t.Fatalf("lost recorded blocker identifiers: %+v", tickets)
	}
}

// The fixture is the real worker-list of Run run_9714ea9b8576: dispatch IDs
// and statuses must parse as recorded, and no settled row may be abandoned.
func TestRecordedDispatchesWorkerListLiveness(t *testing.T) {
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-list-liveness.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{Orca: &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{envelope.Result, envelope.Result}}}}
	rows, err := r.dispatches(context.Background(), "run_9714ea9b8576")
	if err != nil {
		t.Fatal(err)
	}
	want := []dispatchRow{{"ctx_f9e3c9ac102a", "failed", ""}, {"ctx_6d1316722a7c", "failed", ""}, {"ctx_a93f6b74d60d", "failed", ""}, {"ctx_2ec61f77c499", "completed", ""}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v", rows)
	}
	for _, row := range rows {
		if !settledDispatch(row.Status) {
			t.Fatalf("recorded row %+v counted as unsettled", row)
		}
	}
	r.StateRoot = t.TempDir()
	r.PIDAlive = func(int) bool { return false }
	r.Repositories = map[string]config.Repository{"/repo": {Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/proj"}}}
	ticket, ledger := reapLedger(t, "阶段: worker 运行中\n归属: 执行器 term=- run=run_9714ea9b8576 attempt=1")
	r.Clients = ledger
	if err := orca.WriteBeat(orca.HeartbeatPath(r.StateRoot, "proj", "OF-1", "executor"), orca.Beat{PID: 7, Run: "run_9714ea9b8576", At: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReapDead(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if abandoned := r.Orca.(*reapOrca).abandoned; len(abandoned) != 0 {
		t.Fatalf("settled dispatches abandoned: %v", abandoned)
	}
}

// An abandoned dispatch reads as dispatchStatus failed; only workerState names
// the abandon a failed terminal close must retry (orca-worker-list-abandoned.json).
func TestRecordedDispatchesWorkerListAbandoned(t *testing.T) {
	var envelope struct {
		Result orca.Receipt `json:"result"`
	}
	if err := json.Unmarshal(recorded(t, "orca-worker-list-abandoned.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{Orca: &reapOrca{fakeOrca: fakeOrca{pages: []orca.Receipt{envelope.Result}}}}
	rows, err := r.dispatches(context.Background(), "run_4eb9fdf9ecdc")
	if err != nil {
		t.Fatal(err)
	}
	if want := []dispatchRow{{"ctx_ee6da2ada658", "failed", "abandoned"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%+v", rows)
	}
}

// The samples are real git output for a worktree whose uninitialized gitlink sub/
// holds a local file: --others never lists sub/notes, so the archive must take
// sub from the --cached list.
func TestRecordedGitLsFilesGitlinkArchive(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{".gitignore": ".omc/\n", "tracked": "original\n", "untracked": "u\n", "sub/notes": "notes\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	samples := map[string][]byte{
		"--cached": recorded(t, "git-ls-files-cached-gitlink.bin"),
		"--others": recorded(t, "git-ls-files-others-gitlink.bin"),
	}
	runtime := &Runtime{Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
		if spec.Path != "git" || len(spec.Args) < 2 || spec.Args[0] != "ls-files" || samples[spec.Args[1]] == nil {
			t.Fatalf("unexpected command %s %v", spec.Path, spec.Args)
		}
		return samples[spec.Args[1]], nil
	}}
	files, err := runtime.workspaceArchiveFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(t.TempDir(), "list")
	if err := os.WriteFile(list, files, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "worktree.tar")
	if out, err := runx.Run(context.Background(), runx.Spec{Path: "tar", Args: []string{"-cf", archive, "--null", "-T", list}, Dir: root, Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	members, err := runx.Run(context.Background(), runx.Spec{Path: "tar", Args: []string{"-tf", archive}, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sub/notes", "tracked", "untracked", ".gitignore"} {
		if !slices.Contains(strings.Split(string(members), "\n"), want) {
			t.Fatalf("archive members %q miss %s", members, want)
		}
	}
}

// The sample is this host's real /proc/self/mountinfo: /run is tmpfs 0:27 and
// /run/snapd/ns is a bind mount of its subdirectory /snapd/ns, so paths under
// the bind mount resolve to sources inside /run/snapd.
func TestRecordedMountInfoBindSubdir(t *testing.T) {
	mounts, err := parseMountInfo(string(recorded(t, "proc-self-mountinfo-bind-subdir.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if bind := (mount{device: "0:27", root: "/snapd/ns", point: "/run/snapd/ns"}); !slices.Contains(mounts, bind) {
		t.Fatalf("recorded bind mount %+v missing from parsed table", bind)
	}
	if device, source := mountSource(mounts, "/run/snapd/ns/auto-cpufreq.mnt"); device != "0:5" || source != "mnt:[4026532949]" {
		t.Fatalf("mount over the bind mount resolved to device %q source %q", device, source)
	}
	backupDevice, backupSource := mountSource(mounts, "/run/snapd/ns/x")
	if backupDevice != "0:27" || backupSource != "/snapd/ns/x" {
		t.Fatalf("bind mount path resolved to device %q source %q", backupDevice, backupSource)
	}
	workspaceDevice, workspaceSource := mountSource(mounts, "/run/snapd")
	if workspaceDevice != "0:27" || workspaceSource != "/snapd" {
		t.Fatalf("/run/snapd resolved to device %q source %q", workspaceDevice, workspaceSource)
	}
	if !pathWithin(backupSource, workspaceSource) {
		t.Fatalf("source %q must lie inside %q", backupSource, workspaceSource)
	}
	if device, source := mountSource(mounts, "/run/user/1000/doc/f"); device != "0:65" || source != "/f" {
		t.Fatalf("nested mount resolved to device %q source %q", device, source)
	}
}
