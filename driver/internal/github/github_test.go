package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

func fixture(t *testing.T, h http.HandlerFunc, options Options) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	options.BaseURL = s.URL
	if options.Token == nil {
		options.Token = func(context.Context) (string, error) { return "test-secret", nil }
	}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreatePRIdempotent(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			var gets, posts atomic.Int64
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/app/pulls" {
					t.Errorf("path %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("missing authorization")
				}
				if r.Method == "GET" {
					gets.Add(1)
					if r.URL.Query().Get("head") != "acme:topic" || r.URL.Query().Get("state") != "open" {
						t.Error("wrong search")
					}
					if exists || posts.Load() > 0 {
						if _, err := fmt.Fprint(w, `[{"number":17,"head":{"sha":"abc"}}]`); err != nil {
							t.Error(err)
							return
						}
					} else {
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Error(err)
							return
						}
					}
					return
				}
				posts.Add(1)
				// The server accepted the write but its response times out.
				select {
				case <-r.Context().Done():
				case <-time.After(100 * time.Millisecond):
				}
			}, Options{HTTPClient: &http.Client{Timeout: 30 * time.Millisecond}})
			pr, err := c.CreatePR(context.Background(), "acme", "app", CreatePRRequest{Head: "acme:topic", Base: "main", Title: "A change"})
			if err != nil || pr.Number != 17 {
				t.Fatalf("pr=%+v err=%v", pr, err)
			}
			getCount, postCount := gets.Load(), posts.Load()
			if exists && (postCount != 0 || getCount != 1) {
				t.Fatalf("gets=%d posts=%d", getCount, postCount)
			}
			if !exists && (postCount != 1 || getCount != 2) {
				t.Fatalf("gets=%d posts=%d", getCount, postCount)
			}
		})
	}
}

func TestMergeReadBack(t *testing.T) {
	for _, merged := range []bool{true, false} {
		t.Run(fmt.Sprint(merged), func(t *testing.T) {
			reads := 0
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if r.URL.Path != "/repos/acme/app/pulls/17/merge" || body["merge_method"] != "squash" || body["sha"] != "abc" {
						t.Error("wrong merge request")
					}
					if _, err := fmt.Fprint(w, `{"merged":true}`); err != nil {
						t.Error(err)
						return
					}
				} else {
					reads++
					if _, err := fmt.Fprintf(w, `{"number":17,"merged":%t,"mergeable_state":"clean","head":{"sha":"abc"}}`, merged); err != nil {
						t.Error(err)
						return
					}
				}
			}, Options{})
			pr, err := c.MergePR(context.Background(), "acme", "app", 17, "abc")
			if reads != 1 {
				t.Fatalf("reads=%d", reads)
			}
			if merged && (err != nil || !pr.Merged || pr.Head.SHA != "abc" || pr.MergeableState != "clean") {
				t.Fatalf("pr=%+v err=%v", pr, err)
			}
			if !merged && !errors.Is(err, infra.ErrWriteUnconfirmed) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestETag(t *testing.T) {
	calls := 0
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("ETag", `"v1"`)
			if _, err := fmt.Fprint(w, `{"number":17,"head":{"sha":"abc"}}`); err != nil {
				t.Error(err)
				return
			}
			return
		}
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Error("missing conditional header")
		}
		w.WriteHeader(http.StatusNotModified)
	}, Options{})
	for i := 0; i < 2; i++ {
		pr, err := c.GetPR(context.Background(), "acme", "app", 17)
		if err != nil || pr.Number != 17 || pr.Head.SHA != "abc" {
			t.Fatalf("pr=%+v err=%v", pr, err)
		}
	}
}

func TestWriteSpacingAndEndpoints(t *testing.T) {
	now := time.Unix(100, 0)
	var writes []time.Time
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes = append(writes, now)
		}
		switch r.URL.Path {
		case "/repos/acme/app/issues/1/comments":
			if _, err := fmt.Fprint(w, `{"id":42}`); err != nil {
				t.Error(err)
				return
			}
		case "/repos/acme/app/compare/main...topic":
			if _, err := fmt.Fprint(w, `{"files":[{"filename":"a.go","status":"modified"}]}`); err != nil {
				t.Error(err)
				return
			}
		case "/repos/acme/app/commits/abc/check-runs":
			if _, err := fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":3,"name":"ci","status":"completed","conclusion":"success","head_sha":"abc"}]}`); err != nil {
				t.Error(err)
				return
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}, Options{Now: func() time.Time { return now }, Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }})
	ctx := context.Background()
	for range 2 {
		if err := c.request(ctx, http.MethodPost, "/repos/acme/app/issues/1/comments", map[string]string{"body": "x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(writes) != 2 || writes[1].Sub(writes[0]) < time.Second {
		t.Fatalf("writes=%v", writes)
	}
	cmp, err := c.Compare(ctx, "acme", "app", "main", "topic")
	if err != nil || len(cmp.Files) != 1 || cmp.Files[0].Filename != "a.go" {
		t.Fatalf("compare=%v err=%v", cmp, err)
	}
	checks, err := c.CheckRuns(ctx, "acme", "app", "abc")
	if err != nil || len(checks) != 1 || checks[0].Conclusion != "success" {
		t.Fatalf("checks=%v err=%v", checks, err)
	}
}

func TestErrorsAndRedirectDoNotLeakToken(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential leaked across origin")
		}
		if _, err := fmt.Fprint(w, `{"number":17}`); err != nil {
			t.Error(err)
			return
		}
	}))
	defer foreign.Close()
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, http.StatusFound) }, Options{})
	_, err := c.GetPR(context.Background(), "acme", "app", 17)
	if err == nil {
		t.Fatal("redirect should be rejected")
	}
	for _, code := range []int{401, 403, 500} {
		c = fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			if _, err := fmt.Fprint(w, "test-secret"); err != nil {
				t.Error(err)
				return
			}
		}, Options{})
		_, err = c.GetPR(context.Background(), "acme", "app", 17)
		if err == nil || strings.Contains(fmt.Sprintf("%+v", err), "test-secret") {
			t.Fatal("unsafe or missing error")
		}
		if code == 401 || code == 403 {
			var global *infra.Global
			if !errors.As(err, &global) || global.Dep != infra.GitHub {
				t.Fatal("expected GitHub global failure")
			}
		}
	}
	c = fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("request after token failure") }, Options{Token: func(context.Context) (string, error) { return "", errors.New("test-secret") }})
	_, err = c.GetPR(context.Background(), "acme", "app", 17)
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatal("unsafe token error")
	}
}

func TestBareHeadIsQualified(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head") != "acme:topic" {
			t.Errorf("head=%q, want acme:topic", r.URL.Query().Get("head"))
		}
		if _, err := fmt.Fprint(w, `[{"number":17}]`); err != nil {
			t.Error(err)
			return
		}
	}, Options{})
	pr, err := c.FindOpenPR(context.Background(), "acme", "app", "topic")
	if err != nil || pr == nil || pr.Number != 17 {
		t.Fatalf("pr=%+v err=%v", pr, err)
	}
}

func TestFailedWriteInvalidatesETag(t *testing.T) {
	writes := 0
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writes++
			w.WriteHeader(500)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Error("recovery read used stale cache")
		}
		w.Header().Set("ETag", `"before"`)
		if writes == 0 {
			if _, err := fmt.Fprint(w, `[]`); err != nil {
				t.Error(err)
				return
			}
		} else {
			if _, err := fmt.Fprint(w, `[{"number":17}]`); err != nil {
				t.Error(err)
				return
			}
		}
	}, Options{})
	pr, err := c.CreatePR(context.Background(), "acme", "app", CreatePRRequest{Head: "acme:topic", Base: "main", Title: "change"})
	if err != nil || pr.Number != 17 || writes != 1 {
		t.Fatalf("pr=%+v err=%v writes=%d", pr, err, writes)
	}
}

func TestAllWriteVerbsAreThrottled(t *testing.T) {
	now := time.Unix(100, 0)
	var stamps []time.Time
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) { stamps = append(stamps, now); w.WriteHeader(204) }, Options{Now: func() time.Time { return now }, Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }})
	for _, verb := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if err := c.request(context.Background(), verb, "/write", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(stamps) != 4 {
		t.Fatalf("writes=%d", len(stamps))
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i].Sub(stamps[i-1]) < time.Second {
			t.Fatal("writes too close")
		}
	}
}
