package linear

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestProjectionFailedPublishKeepsCompleteSnapshot(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	want := Issue{ID: "issue-1", Identifier: "ENG-1", UpdatedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	if err := p.Publish(context.Background(), ProjectionSnapshot{Issues: []Issue{want}, Complete: true, Watermark: "2026-10-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), ProjectionSnapshot{Issues: nil, Complete: false, Watermark: "bad"}); err == nil {
		t.Fatal("incomplete snapshot was published")
	}
	got, err := p.Read(context.Background(), ReadOptions{RequireComplete: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Issues) != 1 || got.Issues[0].Identifier != want.Identifier || !got.Meta.Complete || got.Meta.Watermark != "2026-10-07T00:00:00Z" || got.Meta.Source != ProjectionSource {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestWebhookAcceptsIssueAndDeduplicatesDelivery(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Date(2026, 10, 7, 4, 0, 0, 0, time.UTC)
	h := NewWebhookHandler(p, WebhookOptions{Secret: "secret", PublicURL: "https://hooks.example.test/linear", Now: func() time.Time { return now }})
	body := []byte(`{"action":"create","type":"Issue","data":{"id":"issue-1","identifier":"ENG-1"}}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, signedWebhookRequest(t, http.MethodPost, "/", body, "delivery-1", "secret", now))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, signedWebhookRequest(t, http.MethodPost, "/", body, "delivery-1", "secret", now))
	if rr.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d", rr.Code)
	}
	var count int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM projection_delivery`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("deliveries=%d", count)
	}
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM projection_dirty WHERE object_type='issue' AND object_id='issue-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("dirty=%d", count)
	}
}

func TestWebhookRejectsInvalidTimestampAndStorageFailure(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Date(2026, 10, 7, 4, 0, 0, 0, time.UTC)
	h := NewWebhookHandler(p, WebhookOptions{Secret: "secret", PublicURL: "https://hooks.example.test/linear", Now: func() time.Time { return now }})
	body := []byte(`{"action":"update","type":"Comment","data":{"id":"comment-1","issue":{"id":"issue-1"}}}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, signedWebhookRequest(t, http.MethodPost, "/", body, "delivery-2", "secret", now.Add(-time.Hour)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("stale status=%d", rr.Code)
	}
	if err := p.db.Close(); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, signedWebhookRequest(t, http.MethodPost, "/", body, "delivery-3", "secret", now))
	if rr.Code == http.StatusOK {
		t.Fatal("storage failure acknowledged success")
	}
}

func TestWebhookReportsDegradedWithoutPublicHTTPSReceiver(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	h := NewWebhookHandler(p, WebhookOptions{Secret: "secret"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rr.Code)
	}
	if h.Mode() != WebhookDegraded {
		t.Fatalf("mode=%s", h.Mode())
	}
}

func signedWebhookRequest(t *testing.T, method, path string, body []byte, delivery, secret string, at time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Linear-Delivery", delivery)
	req.Header.Set("Linear-Webhook-Timestamp", strconv.FormatInt(at.Unix(), 10))
	sum := hmac.New(sha256.New, []byte(secret))
	sum.Write(body)
	req.Header.Set("Linear-Signature", hex.EncodeToString(sum.Sum(nil)))
	return req
}

func TestWebhookAcceptsCommentAndRejectsBadSignature(t *testing.T) {
	p, err := OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team-1/project-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Date(2026, 10, 7, 4, 0, 0, 0, time.UTC)
	h := NewWebhookHandler(p, WebhookOptions{Secret: "secret", PublicURL: "https://hooks.example.test/linear", Now: func() time.Time { return now }})
	body := []byte(`{"action":"update","type":"Comment","data":{"id":"comment-1","body":"hello","issue":{"id":"issue-1"}}}`)
	req := signedWebhookRequest(t, http.MethodPost, "/", body, "delivery-comment", "wrong", now)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status=%d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, signedWebhookRequest(t, http.MethodPost, "/", body, "delivery-comment", "secret", now))
	if rr.Code != http.StatusOK {
		t.Fatalf("comment status=%d", rr.Code)
	}
	dirty, err := p.Dirty(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty["comment"]) != 1 || dirty["comment"][0] != "comment-1" || len(dirty["issue"]) != 1 || dirty["issue"][0] != "issue-1" {
		t.Fatalf("dirty=%v", dirty)
	}
}
