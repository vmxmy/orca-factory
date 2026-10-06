package issues

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
)

func TestPersistentConcurrentClientsCoalesce(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var reads atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			close(entered)
		}
		<-release
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open"}]`, taskBody)
	}))
	defer s.Close()
	results := make(chan error, 8)
	for range 8 {
		gh, _ := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
		c, _ := NewGitHub("o/r", gh)
		go func() { _, err := c.List(context.Background()); results <- err }()
	}
	<-entered
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range 8 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("concurrent independent refreshes=%d; want1", reads.Load())
	}
}

func TestPersistentMutationInvalidatesAmbiguousWrite(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var changed atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			changed.Store(true)
			http.Error(w, "response lost", http.StatusServiceUnavailable)
			return
		}
		state := "open"
		if changed.Load() {
			state = "closed"
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":%q}]`, taskBody, state)
	}))
	defer s.Close()
	newClient := func() *GitHub {
		gh, _ := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
		c, _ := NewGitHub("o/r", gh)
		return c
	}
	a, b := newClient(), newClient()
	if _, err := a.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UpdateIssue(context.Background(), "ZII-42", "Done", nil, nil); err == nil {
		t.Fatal("ambiguous write error lost")
	}
	rows, err := newClient().List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].State.Name != "Done" {
		t.Fatalf("cross-client cache survived ambiguous mutation: %+v %v", rows, err)
	}
}

func TestPersistentMutationFencesOtherAuthorizationScopes(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var changed atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			changed.Store(true)
			http.Error(w, "response lost", http.StatusServiceUnavailable)
			return
		}
		state := "open"
		if changed.Load() {
			state = "closed"
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":%q}]`, taskBody, state)
	}))
	defer s.Close()
	newClient := func(token string) *GitHub {
		gh, _ := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return token, nil }})
		c, _ := NewGitHub("o/r", gh)
		return c
	}
	a, b := newClient("reader"), newClient("writer")
	if _, err := a.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UpdateIssue(context.Background(), "ZII-42", "Done", nil, nil); err == nil {
		t.Fatal("ambiguous write error lost")
	}
	rows, err := newClient("reader").List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].State.Name != "Done" {
		t.Fatalf("cross-client cache survived ambiguous mutation: %+v %v", rows, err)
	}
}

func TestPersistentTTLStrongReadsAndScopeIsolation(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var version, reads, commentReads atomic.Int32
	version.Store(1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			commentReads.Add(1)
			_, _ = fmt.Fprintf(w, `[{"id":3,"body":"history%d","created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`, version.Load())
			return
		}
		reads.Add(1)
		repo := "o/r"
		if strings.Contains(r.URL.Path, "/other/") {
			repo = "o/other"
		}
		body := strings.ReplaceAll(taskBody, `"project":"o/r"`, `"project":"`+repo+`"`)
		title := "first"
		if r.Header.Get("Authorization") != "Bearer first-secret" {
			title = "second"
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"title":%q,"state":"open","updated_at":"2026-10-06T00:00:00Z"}]`, body, title)
	})
	s, otherHost := httptest.NewServer(handler), httptest.NewServer(handler)
	defer s.Close()
	defer otherHost.Close()
	var token atomic.Value
	token.Store("first-secret")
	var tokenCalls atomic.Int32
	newClient := func(host, repo string) *GitHub {
		gh, _ := github.New(github.Options{BaseURL: host, Token: func(context.Context) (string, error) { tokenCalls.Add(1); return token.Load().(string), nil }})
		c, err := NewGitHub(repo, gh)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := newClient(s.URL, "o/r")
	clock := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	c.cache.Now = func() time.Time { return clock }
	ctx := context.Background()
	for range 2 {
		if _, err := c.List(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 || tokenCalls.Load() != 2 {
		t.Fatalf("captured auth or TTL: reads=%d auth=%d", reads.Load(), tokenCalls.Load())
	}
	for range 2 {
		if _, err := c.SnapshotComments(ctx, "ZII-42"); err != nil {
			t.Fatal(err)
		}
	}
	version.Store(2)
	cached, err := c.SnapshotComments(ctx, "ZII-42")
	if err != nil || cached[0].Body != "history1" {
		t.Fatalf("bounded comment reuse: %+v %v", cached, err)
	}
	fresh, err := c.ListComments(ctx, "ZII-42")
	if err != nil || fresh[0].Body != "history2" {
		t.Fatalf("strong comments used cache: %+v %v", fresh, err)
	}
	before := reads.Load()
	if _, err = c.Read(ctx, "ZII-42"); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != before+1 {
		t.Fatal("Read bypassed remote")
	}
	clock = clock.Add(31 * time.Second)
	if _, err = c.List(ctx); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != before+2 {
		t.Fatal("metadata TTL not repaired")
	}
	clock = clock.Add(5 * time.Minute)
	history, err := c.SnapshotComments(ctx, "ZII-42")
	if err != nil || history[0].Body != "history2" || commentReads.Load() != 3 {
		t.Fatalf("independent comments TTL: %+v %v reads=%d", history, err, commentReads.Load())
	}
	token.Store("second-secret")
	rows, err := c.List(ctx)
	if err != nil || rows[0].Title != "second" {
		t.Fatalf("auth rotation reused projection: %+v %v", rows, err)
	}
	before = reads.Load()
	for _, client := range []*GitHub{newClient(otherHost.URL, "o/r"), newClient(s.URL, "o/other")} {
		if _, err = client.List(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != before+2 {
		t.Fatal("host/repo scope reused foreign projection")
	}
	raw, err := os.ReadFile(c.cache.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "first-secret") || strings.Contains(string(raw), "second-secret") {
		t.Fatal("credential persisted in cache")
	}
	_, scope, err := c.gh.Scope(ctx, "o/r")
	if err != nil || strings.Contains(scope, "secret") {
		t.Fatalf("plaintext credential in scope: %s %v", scope, err)
	}
}

func TestPersistentFailedPaginationKeepsCompleteGeneration(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var broken atomic.Bool
	var host string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			if r.URL.Query().Get("page") == "2" {
				http.Error(w, "page2 failed", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, host, r.URL.Path))
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"title":"complete generation","body":%q,"state":"open"}]`, taskBody)
	}))
	defer s.Close()
	host = s.URL
	gh, _ := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	c, _ := NewGitHub("o/r", gh)
	clock := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	c.cache.Now = func() time.Time { return clock }
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	clock = clock.Add(31 * time.Second)
	rows, err := c.List(context.Background())
	if err == nil || rows != nil {
		t.Fatalf("partial pagination admitted: %+v %v", rows, err)
	}
	clock = clock.Add(-11 * time.Second)
	rows, err = c.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Title != "complete generation" {
		t.Fatalf("failed fetch advanced progress: %+v %v", rows, err)
	}
	broken.Store(false)
	clock = clock.Add(11 * time.Second)
	if _, err = c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentProcessHelper(t *testing.T) {
	if os.Getenv("CACHE_TEST_PROCESS") != "1" {
		return
	}
	gh, err := github.New(github.Options{BaseURL: os.Getenv("CACHE_TEST_HOST"), Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewGitHub("o/r", gh)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Identifier != "ZII-42" {
		t.Fatalf("child snapshot: %+v %v", rows, err)
	}
	history, err := c.SnapshotComments(context.Background(), "ZII-42")
	if err != nil || len(history) != 1 || history[0].Body != "ownership" {
		t.Fatalf("child history: %+v %v", history, err)
	}
}

func TestPersistentSubprocessConcurrencyAndReopen(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var issues, comments atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			comments.Add(1)
			_, _ = fmt.Fprint(w, `[{"id":3,"body":"ownership","created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`)
			return
		}
		issues.Add(1)
		time.Sleep(100 * time.Millisecond)
		_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open","updated_at":"2026-10-06T00:00:00Z"}]`, taskBody)
	}))
	defer s.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPersistentProcessHelper$")
		cmd.Env = append(os.Environ(), "CACHE_TEST_PROCESS=1", "CACHE_TEST_HOST="+s.URL)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("child: %w: %s", err, out)
		}
		return nil
	}
	results := make(chan error, 3)
	for range 3 {
		go func() { results <- run() }()
	}
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if issues.Load() != 1 || comments.Load() != 1 {
		t.Fatalf("four process runs: issues=%d comments=%d; want1 each", issues.Load(), comments.Load())
	}
	t.Logf("four real subprocesses: issue requests=%d comment requests=%d versus uncached4 each", issues.Load(), comments.Load())
}
