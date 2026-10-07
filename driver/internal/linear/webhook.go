package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WebhookMode describes whether a public receiver is configured.
type WebhookMode string

const (
	WebhookReady    WebhookMode = "ready"
	WebhookDegraded WebhookMode = "degraded"
)

var (
	ErrWebhookSignature = errors.New("invalid webhook signature")
	ErrWebhookTimestamp = errors.New("invalid webhook timestamp")
	ErrWebhookDelivery  = errors.New("missing webhook delivery id")
)

type WebhookOptions struct {
	Secret          string
	PublicURL       string
	MaxTimestampAge time.Duration
	Now             func() time.Time
}

type WebhookHandler struct {
	projection *Projection
	secret     []byte
	publicURL  string
	maxAge     time.Duration
	now        func() time.Time
}

func NewWebhookHandler(p *Projection, options WebhookOptions) *WebhookHandler {
	maxAge := options.MaxTimestampAge
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &WebhookHandler{projection: p, secret: []byte(options.Secret), publicURL: options.PublicURL, maxAge: maxAge, now: now}
}

func (h *WebhookHandler) Mode() WebhookMode {
	if !validPublicHTTPS(h.publicURL) {
		return WebhookDegraded
	}
	return WebhookReady
}

func validPublicHTTPS(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Mode() == WebhookDegraded {
		http.Error(w, "linear webhook receiver is degraded: public HTTPS receiver is not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read webhook body", http.StatusBadRequest)
		return
	}
	if err := h.authenticate(r, body); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrWebhookDelivery) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil || event.Type == "" {
		http.Error(w, "invalid webhook payload", http.StatusBadRequest)
		return
	}
	if event.Type != "Issue" && event.Type != "Comment" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.projection.applyWebhook(r.Context(), r.Header.Get("Linear-Delivery"), event, body, h.now().UTC()); err != nil {
		http.Error(w, "webhook storage failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *WebhookHandler) authenticate(r *http.Request, body []byte) error {
	delivery := strings.TrimSpace(r.Header.Get("Linear-Delivery"))
	if delivery == "" {
		return ErrWebhookDelivery
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("Linear-Webhook-Timestamp")), 10, 64)
	if err != nil || absDuration(h.now().Sub(time.Unix(timestamp, 0))) > h.maxAge {
		return ErrWebhookTimestamp
	}
	provided := strings.TrimSpace(r.Header.Get("Linear-Signature"))
	provided = strings.TrimPrefix(provided, "sha256=")
	want := hmac.New(sha256.New, h.secret)
	_, _ = want.Write(body)
	expected := hex.EncodeToString(want.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(provided)), []byte(expected)) {
		return ErrWebhookSignature
	}
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

type webhookEvent struct {
	Action string          `json:"action"`
	Type   string          `json:"type"`
	Data   json.RawMessage `json:"data"`
}

type webhookComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Issue     struct {
		ID string `json:"id"`
	} `json:"issue"`
}

func (p *Projection) applyWebhook(ctx context.Context, delivery string, event webhookEvent, raw []byte, received time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO projection_delivery(dedupe_key, delivered_at) VALUES(?,?)`, delivery, received.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projection_meta SET complete=0`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO projection_events(delivery_id,event_type,action,payload,received_at) VALUES(?,?,?,?,?)`, delivery, event.Type, event.Action, raw, received.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var objectID, issueID string
	switch event.Type {
	case "Issue":
		var issue Issue
		if err := json.Unmarshal(event.Data, &issue); err != nil || issue.ID == "" {
			return fmt.Errorf("invalid issue webhook: %w", err)
		}
		objectID = issue.ID
		if event.Action == "remove" || event.Action == "delete" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM projection_issues WHERE id=?`, objectID); err != nil {
				return err
			}
		} else {
			payload, _ := json.Marshal(issue)
			if _, err = tx.ExecContext(ctx, `INSERT INTO projection_issues(id,payload,seen_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, seen_at=excluded.seen_at`, objectID, payload, received.Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	case "Comment":
		var comment webhookComment
		if err := json.Unmarshal(event.Data, &comment); err != nil || comment.ID == "" || comment.Issue.ID == "" {
			return errors.New("invalid comment webhook")
		}
		objectID, issueID = comment.ID, comment.Issue.ID
		if event.Action == "remove" || event.Action == "delete" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM projection_comments WHERE id=?`, objectID); err != nil {
				return err
			}
		} else {
			payload, _ := json.Marshal(Comment{ID: comment.ID, Body: comment.Body, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
			if _, err = tx.ExecContext(ctx, `INSERT INTO projection_comments(id,issue_id,payload,seen_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET issue_id=excluded.issue_id, payload=excluded.payload, seen_at=excluded.seen_at`, objectID, issueID, payload, received.Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO projection_dirty(object_type,object_id) VALUES(?,?)`, strings.ToLower(event.Type), objectID); err != nil {
		return err
	}
	if issueID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO projection_dirty(object_type,object_id) VALUES('issue',?)`, issueID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Dirty returns object keys changed by accepted webhook deliveries.
func (p *Projection) Dirty(ctx context.Context) (map[string][]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT object_type, object_id FROM projection_dirty ORDER BY object_type, object_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]string{}
	for rows.Next() {
		var typ, id string
		if err := rows.Scan(&typ, &id); err != nil {
			return nil, err
		}
		result[typ] = append(result[typ], id)
	}
	return result, rows.Err()
}
