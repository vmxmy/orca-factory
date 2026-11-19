package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func invokeGear(t *testing.T, args ...string) []byte {
	t.Helper()
	var code int
	out := captureStdout(t, func() { code = run(append([]string{"gear"}, args...)) })
	if code != 0 {
		t.Fatalf("gear %v: exit %d, output %s", args, code, out)
	}
	return out
}

type persistedGear struct {
	Path, Mode, Operator, Reason string
	UpdatedAt                    time.Time
}

func newGearFixture(t *testing.T) onboardFixture {
	t.Helper()
	f := newOnboardFixture(t)
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, f.factory, "github.com/vmxmy/orca-factory"))
	return f
}

func readGearEntries(t *testing.T, file string) map[string]persistedGear {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]persistedGear
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestGearCommands(t *testing.T) {
	f := newGearFixture(t)
	ledger, server := newWizardFeishuServer(t)
	ledger.rows = []feishuFakeRow{{id: "rec1", cells: map[string]any{
		"名称": "orca-factory", "仓库远端": "github.com/vmxmy/orca-factory", "编号前缀": "OF", "启用": true,
	}}}
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")
	writeExecutable(t, filepath.Join(f.factory, "bin", "orca-factory"), "#!/bin/sh\nexit 0\n")
	profile := filepath.Join(f.factory, "scripts/agent-gates/agent-graph.env")
	original, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	status := func(mode, source string) {
		t.Helper()
		var rows []struct{ Remote, Prefix, Path, Mode, Source string }
		out := invokeGear(t, "status", "--json")
		if err := json.Unmarshal(out, &rows); err != nil {
			t.Fatalf("status: %s %v", out, err)
		}
		if len(rows) != 1 || rows[0].Mode != mode || rows[0].Source != source || rows[0].Prefix != "OF" || rows[0].Remote != "github.com/vmxmy/orca-factory" || rows[0].Path != f.factory {
			t.Fatalf("status=%+v, want %s %s", rows, mode, source)
		}
	}
	status("manual", "default")
	invokeGear(t, "set", "OF", "auto", "--operator", "tester", "--reason", "resume queue")
	status("auto", "override")
	file := filepath.Join(f.state, "gear.json")
	gear := readGearEntries(t, file)["github.com/vmxmy/orca-factory"]
	if gear.Path != f.factory || gear.Mode != "auto" || gear.Operator != "tester" || gear.Reason != "resume queue" || gear.UpdatedAt.IsZero() {
		t.Fatalf("persisted gear=%+v", gear)
	}
	invokeGear(t, "set", f.factory, "manual", "--operator", "tester", "--reason", "pause queue")
	status("manual", "override")
	if got, err := config.LoadRegisteredRepository("github.com/vmxmy/orca-factory", f.factory); err != nil || got.Env["AGENT_MODE"] != "manual" || got.Sources["AGENT_MODE"] != "override" {
		t.Fatalf("loaded=%+v error=%v", got, err)
	}
	invokeGear(t, "clear", "OF")
	status("manual", "default")
	for _, args := range [][]string{
		{"set", "OF", "auto"},
		{"set", "OF", "invalid", "--operator", "tester", "--reason", "bad"},
		{"set", "OF", "auto", "--operator", " ", "--reason", "bad"},
		{"status", "--bad"},
		{"clear"},
	} {
		if code := run(append([]string{"gear"}, args...)); code != 2 {
			t.Fatalf("gear %v exit=%d, want 2", args, code)
		}
	}
	after, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("repository profile changed: %v", err)
	}
	other := fmt.Sprintf(`{ "path":%q, "mode":null, "note":"<leave intact>" }`, f.factory)
	bad := fmt.Sprintf(`{"mode":"bogus","path":%q}`, f.factory)
	t.Chdir(f.factory)
	for _, tc := range []struct{ selector, entries string }{
		{"OF", fmt.Sprintf(`{"github.com/vmxmy/orca-factory":%s,"other/repo":%s}`, bad, other)},
		{".", fmt.Sprintf(`{"github.com/vmxmy/orca-factory":%s,"other/repo":%s}`, bad, other)},
	} {
		writeTestFile(t, file, tc.entries)
		invokeGear(t, "clear", tc.selector)
		status("manual", "default")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var remaining map[string]json.RawMessage
		if err := json.Unmarshal(data, &remaining); err != nil || len(remaining) != 1 || string(remaining["other/repo"]) != other {
			t.Fatalf("after clearing %q: %s, err=%v", tc.selector, data, err)
		}
	}
	ledger.rows[0].cells["启用"] = false
	invokeGear(t, "set", f.factory, "auto", "--operator", "tester", "--reason", "disabled project with in-flight tickets")
	status("auto", "override")
	invokeGear(t, "clear", f.factory)
	ledger.rows[0].cells["启用"] = true
	server.Close()
	invokeGear(t, "set", f.factory, "manual", "--operator", "t", "--reason", "r")
	entries := readGearEntries(t, file)
	if got := entries["github.com/vmxmy/orca-factory"]; len(entries) != 2 || got.Mode != "manual" || got.Path != f.factory {
		t.Fatalf("set by path with Feishu offline: %+v", entries)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.factory, alias); err != nil {
		t.Fatal(err)
	}
	invokeGear(t, "set", alias, "auto", "--operator", "t", "--reason", "r")
	if got := readGearEntries(t, file)["github.com/vmxmy/orca-factory"]; got.Mode != "auto" || got.Path != f.factory {
		t.Fatalf("set through symlink alias: %+v", got)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "sub"), filepath.Join(f.factory, "link")); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"gear", "clear", filepath.Join(f.factory, "link") + "/.."}); code != 1 {
		t.Fatalf("clear through a link to an unregistered directory exit=%d, want 1", code)
	}
	t.Chdir(f.factory)
	if code := run([]string{"gear", "clear", "link/.."}); code != 1 {
		t.Fatalf("clear relative link/.. exit=%d, want 1", code)
	}
	t.Chdir(filepath.Join(f.factory, "link"))
	if code := run([]string{"gear", "clear", ".."}); code != 1 {
		t.Fatalf("clear .. from a logical PWD through a link exit=%d, want 1", code)
	}
	t.Chdir(filepath.Join(f.factory, "scripts"))
	invokeGear(t, "clear", "..")
	if entries := readGearEntries(t, file); len(entries) != 1 || entries["other/repo"].Path != f.factory {
		t.Fatalf("clear .. with Feishu offline: %+v", entries)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 10 {
		t.Fatalf("logs=%q, want ten changes", logs.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, fmt.Sprintf("path=%q", f.factory)) || !strings.Contains(line, `remote="github.com/vmxmy/orca-factory"`) || !strings.Contains(line, "operator=") || !strings.Contains(line, "reason=") {
			t.Fatalf("audit fields missing: %s", line)
		}
	}
	if !strings.Contains(lines[0], `operator="tester"`) || !strings.Contains(lines[0], `reason="resume queue"`) {
		t.Fatalf("set audit=%s", lines[0])
	}
}

func TestGearRecordedRemoteDrift(t *testing.T) {
	replay := filepath.Join(t.TempDir(), "orca")
	t.Setenv("ORCA_REPO_RECORDING", recordedPath(t, "orca-repo-list.json"))
	writeExecutable(t, replay, "exec cat \"$ORCA_REPO_RECORDING\"\n")
	repos, err := (&orca.Client{Path: replay}).RepoList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var upstream string
	for _, repo := range repos {
		if filepath.Base(repo.Path) == "dujiao-next" {
			upstream = repo.CanonicalKey
		}
	}
	origin, err := os.ReadFile(recordedPath(t, "git-remote-get-url-dujiao-next.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fork, err := orca.NormalizeRemote(string(origin))
	if err != nil || upstream != "github.com/dujiao-next/dujiao-next" || fork != "github.com/vmxmy/dujiao-next" {
		t.Fatalf("recorded upstream=%q fork=%q err=%v", upstream, fork, err)
	}

	f := newOnboardFixture(t)
	a, b := f.factory, f.repo
	ledger, server := newWizardFeishuServer(t)
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")
	writeExecutable(t, filepath.Join(f.factory, "bin", "orca-factory"), "exit 0\n")
	var rows []string
	for _, repo := range []struct{ path, remote, prefix string }{{a, upstream, "DJ"}, {b, fork, "FORK"}} {
		gitIn(t, repo.path, "init", "--quiet")
		gitIn(t, repo.path, "remote", "add", "origin", strings.TrimSpace(string(origin)))
		writeTestFile(t, filepath.Join(repo.path, "scripts/agent-gates/agent-graph.env"),
			"AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_BASE=BASE_TOKEN\nAGENT_FEISHU_PROJECT="+repo.remote+"\n")
		rows = append(rows, fmt.Sprintf(`{"path":%q,"gitRemoteIdentity":{"canonicalKey":%q}}`, repo.path, repo.remote))
		ledger.rows = append(ledger.rows, feishuFakeRow{id: repo.prefix, cells: map[string]any{
			"名称": repo.prefix, "仓库远端": repo.remote, "编号前缀": repo.prefix, "启用": true,
		}})
	}
	inventory := filepath.Join(t.TempDir(), "repos.json")
	writeTestFile(t, inventory, `{"ok":true,"result":{"repos":[`+strings.Join(rows, ",")+`]}}`)
	t.Setenv("ORCA_REPO_RECORDING", inventory)
	t.Setenv("AGENT_ORCA_BIN", replay)

	set := func(repo, mode string) {
		t.Helper()
		invokeGear(t, "set", repo, mode, "--operator", "tester", "--reason", "drift")
	}
	file := filepath.Join(f.state, "gear.json")
	set("dj", "auto")
	entries := readGearEntries(t, file)
	if len(entries) != 1 || entries[upstream].Mode != "auto" || entries[upstream].Path != a {
		t.Fatalf("set upstream: %+v", entries)
	}
	set("FORK", "manual")
	want := []gearStatus{
		{Remote: upstream, Prefix: "DJ", Path: a, Mode: "auto", Source: "override"},
		{Remote: fork, Prefix: "FORK", Path: b, Mode: "manual", Source: "override"},
	}
	for _, row := range want {
		got, err := config.LoadRegisteredRepository(row.Remote, row.Path)
		if err != nil || got.Env["AGENT_MODE"] != row.Mode || got.Sources["AGENT_MODE"] != row.Source {
			t.Fatalf("load %s: got %q %q, err=%v", row.Path, got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err)
		}
	}
	var status []gearStatus
	if err := json.Unmarshal(invokeGear(t, "status", "--json"), &status); err != nil ||
		len(status) != len(want) || !slices.Contains(status, want[0]) || !slices.Contains(status, want[1]) {
		t.Fatalf("status=%+v want=%+v err=%v", status, want, err)
	}

	beforeFork := readGearEntries(t, file)[fork]
	t.Chdir(b)
	subdir := filepath.Join(b, "DJ")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	set("DJ", "manual")
	entries = readGearEntries(t, file)
	if len(entries) != 2 || entries[upstream].Mode != "manual" || entries[upstream].Path != a || entries[fork] != beforeFork {
		t.Fatalf("set prefix from fork: %+v", entries)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"gear", "set", subdir, "auto", "--operator", "tester", "--reason", "subdir"}); code != 1 {
		t.Fatalf("set subdirectory exit=%d, want 1", code)
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("subdirectory set changed gear.json: %s, err=%v", after, err)
	}
	invokeGear(t, "clear", "DJ")
	if got, err := config.LoadRegisteredRepository(upstream, a); err != nil || got.Sources["AGENT_MODE"] == "override" {
		t.Fatalf("cleared DJ inherited FORK: mode=%q source=%q err=%v", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err)
	}
	if remaining := readGearEntries(t, file); len(remaining) != 1 || remaining[fork] != beforeFork {
		t.Fatalf("clear DJ changed FORK: %+v", remaining)
	}
	invokeGear(t, "clear", "FORK")
	set("DJ", "auto")
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if got, err := config.LoadRegisteredRepository(fork, b); err != nil || got.Sources["AGENT_MODE"] == "override" {
		t.Fatalf("FORK inherited removed DJ checkout: mode=%q source=%q err=%v", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err)
	}
	if got, err := config.LoadRegisteredRepository(upstream, b); err != nil || got.Sources["AGENT_MODE"] == "override" {
		t.Fatalf("DJ gear applied to a checkout whose ledger project is FORK: mode=%q source=%q err=%v", got.Env["AGENT_MODE"], got.Sources["AGENT_MODE"], err)
	}
}

func TestGearSetPrefixRecordedProject(t *testing.T) {
	f := newGearFixture(t)
	writeTestFile(t, filepath.Join(f.home, ".config/orca-factory/driver.toml"), "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"bot-tables-list.json", "bot-project-fields.json", "bot-project-records.json", "fields-list.json", "bot-fields-list-events.json"} {
		raw, err := os.ReadFile(recordedPath(t, filepath.Join("feishu", name)))
		if err != nil {
			t.Fatal(err)
		}
		var recording struct {
			Request  struct{ Method, Path string }
			Response struct{ Data json.RawMessage }
		}
		if err := json.Unmarshal(raw, &recording); err != nil {
			t.Fatal(err)
		}
		mux.HandleFunc(recording.Request.Method+" "+recording.Request.Path, func(w http.ResponseWriter, r *http.Request) {
			feishuEnvelope(t, w, recording.Response.Data)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "test-app")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "test-secret")

	invokeGear(t, "set", "OF", "auto", "--operator", "t", "--reason", "r")
	entries := readGearEntries(t, filepath.Join(f.state, "gear.json"))
	if gear := entries["github.com/vmxmy/orca-factory"]; len(entries) != 1 || gear.Mode != "auto" || gear.Path != f.factory {
		t.Fatalf("recorded OF project: %+v", entries)
	}
}
