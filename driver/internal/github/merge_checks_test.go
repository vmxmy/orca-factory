package github

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

func checkRunsOnly(t *testing.T, body []byte) *Client {
	t.Helper()
	return fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if _, err := w.Write(body); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/status"):
			if _, err := w.Write([]byte(`{"total_count":0,"statuses":[]}`)); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}, Options{})
}

// TestMergeChecksSkippedCountsAsPass replays the real github-check-runs-skipped.json
// recording (PR #447/OF-36: 5 success, 3 skipped): skipped/neutral check-runs count
// toward a green merge decision once at least one check-run succeeded. An all-skipped
// derivative of the same recording, with zero real successes, never counts as passing.
func TestMergeChecksSkippedCountsAsPass(t *testing.T) {
	head := "a8e0c30084e47719036583296dcb4cb32b3bb94b"
	raw, err := os.ReadFile("../../testdata/recorded/github-check-runs-skipped.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("mixed success and skipped is green", func(t *testing.T) {
		c := checkRunsOnly(t, raw)
		result, err := c.MergeChecks(context.Background(), "vmxmy", "orca-factory", head)
		if err != nil || !result.Green || result.Failed {
			t.Fatalf("want green, got %+v err=%v", result, err)
		}
	})
	t.Run("all skipped derivative is not green", func(t *testing.T) {
		allSkipped := strings.ReplaceAll(string(raw), `"conclusion": "success"`, `"conclusion": "skipped"`)
		if allSkipped == string(raw) {
			t.Fatal("replacement did not match fixture formatting")
		}
		c := checkRunsOnly(t, []byte(allSkipped))
		result, err := c.MergeChecks(context.Background(), "vmxmy", "orca-factory", head)
		if err != nil || result.Green || result.Failed {
			t.Fatalf("want not green, got %+v err=%v", result, err)
		}
	})
}
