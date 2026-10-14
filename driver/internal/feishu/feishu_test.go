package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAppID     = "cli_test"
	testAppSecret = "app-secret-value"
	testBase      = "BASE_TOKEN"
	ticketTable   = "tblZQsD8lnjliPcn"
)

type recording struct {
	Request struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	} `json:"request"`
	Response struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code          int      `json:"code"`
			Message       string   `json:"message"`
			MissingScopes []string `json:"missing_scopes"`
		} `json:"error"`
	} `json:"response"`
}

func loadRecording(t *testing.T, name string) recording {
	t.Helper()
	b, err := os.ReadFile("../../testdata/recorded/feishu/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var r recording
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// envelope restores the HTTP body that lark-cli stripped from the recording.
func envelope(t *testing.T, w http.ResponseWriter, code int, data json.RawMessage) {
	t.Helper()
	body := map[string]any{"code": code, "msg": "success"}
	if data != nil {
		body["data"] = data
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Error(err)
	}
}

type fakeServer struct {
	tokens atomic.Int64
}

// newFake answers the token endpoint itself and checks that every other
// request carries the issued token before delegating to h.
func newFake(t *testing.T, h http.HandlerFunc, options Options) (*Client, *fakeServer) {
	t.Helper()
	fake := &fakeServer{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			var in struct {
				AppID     string `json:"app_id"`
				AppSecret string `json:"app_secret"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil || in.AppID != testAppID || in.AppSecret != testAppSecret {
				t.Errorf("token request %s %+v", r.Method, in)
			}
			n := fake.tokens.Add(1)
			if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "t-" + string(rune('0'+n)), "expire": 7200}); err != nil {
				t.Error(err)
			}
			return
		}
		if got, want := r.Header.Get("Authorization"), "Bearer t-"+string(rune('0'+fake.tokens.Load())); got != want {
			t.Errorf("authorization %q want %q", got, want)
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	options.Endpoint = s.URL
	options.AppID = testAppID
	options.AppSecret = testAppSecret
	if options.Sleep == nil {
		options.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

func TestTokenCachedAndRefreshedBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	fields := loadRecording(t, "fields-list.json")
	c, fake := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		envelope(t, w, 0, fields.Response.Data)
	}, Options{Now: func() time.Time { return now }})
	ctx := context.Background()
	for range 2 {
		if _, err := c.Fields(ctx, testBase, ticketTable); err != nil {
			t.Fatal(err)
		}
	}
	if fake.tokens.Load() != 1 {
		t.Fatalf("tokens=%d want cached", fake.tokens.Load())
	}
	now = now.Add(7200*time.Second - time.Minute)
	if _, err := c.Fields(ctx, testBase, ticketTable); err != nil {
		t.Fatal(err)
	}
	if fake.tokens.Load() != 2 {
		t.Fatalf("tokens=%d want refresh before expiry", fake.tokens.Load())
	}
}

func TestNewRejectsMissingCredentials(t *testing.T) {
	if _, err := New(Options{AppID: testAppID}); err == nil {
		t.Fatal("missing secret accepted")
	}
	if _, err := New(Options{AppID: testAppID, AppSecret: testAppSecret, Endpoint: "ftp://x"}); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}

func TestTokenFailureHidesSecret(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope(t, w, 10014, nil)
	}))
	t.Cleanup(s.Close)
	c, err := New(Options{Endpoint: s.URL, AppID: testAppID, AppSecret: testAppSecret})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Fields(context.Background(), testBase, ticketTable)
	if err == nil || strings.Contains(err.Error(), testAppSecret) {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteConflictRetriedWithBackoff(t *testing.T) {
	update := loadRecording(t, "concurrent-update-1.json")
	var calls atomic.Int64
	var delays []time.Duration
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			envelope(t, w, CodeWriteConflict, nil)
			return
		}
		envelope(t, w, 0, update.Response.Data)
	}, Options{Sleep: func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }})
	res, err := c.BatchUpdate(context.Background(), testBase, ticketTable, map[string]map[string]any{"reczz28NLqDRHkSp": {"估算": 1}})
	if err != nil || calls.Load() != 3 || len(delays) != 2 || len(res.RecordIDs) != 1 || res.RecordIDs[0] != "reczz28NLqDRHkSp" {
		t.Fatalf("res=%+v err=%v calls=%d delays=%v", res, err, calls.Load(), delays)
	}
}

func TestWriteConflictExhaustedReturnsError(t *testing.T) {
	var calls atomic.Int64
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		envelope(t, w, CodeWriteConflict, nil)
	}, Options{})
	_, err := c.BatchUpdate(context.Background(), testBase, ticketTable, map[string]map[string]any{"rec1": {"估算": 1}})
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Code != CodeWriteConflict || calls.Load() != 4 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestSameTableWritesSerialized(t *testing.T) {
	update := loadRecording(t, "concurrent-update-1.json")
	var mu sync.Mutex
	inFlight := map[string]int{}
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight[r.URL.Path]++
		overlap := inFlight[r.URL.Path] > 1
		mu.Unlock()
		if overlap {
			t.Errorf("concurrent write to %s", r.URL.Path)
		}
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight[r.URL.Path]--
		mu.Unlock()
		envelope(t, w, 0, update.Response.Data)
	}, Options{})
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := c.BatchUpdate(context.Background(), testBase, ticketTable, map[string]map[string]any{"reczz28NLqDRHkSp": {"估算": 1}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
