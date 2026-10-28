// Package feishu implements the driver's Feishu Base (base/v3) OpenAPI calls.
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

const Dep infra.Dep = "Feishu"

const (
	CodeWriteConflict = 1254291
	CodeMissingScope  = 99991672
	CodeNoPermission  = 91403
	// CodeResponseTooLarge refuses a response body over 10 MiB
	// (bot-events-list-too-large.json); the same request with a smaller page
	// succeeds.
	CodeResponseTooLarge = 800004006
)

const tokenRefreshMargin = 5 * time.Minute

// Options injects credentials, transport, and timing. Endpoint defaults to
// https://open.feishu.cn; an override is a trusted origin (private deployment
// or httptest). Credentials never appear in errors.
type Options struct {
	Endpoint    string
	AppID       string
	AppSecret   string
	HTTPTimeout time.Duration
	HTTPClient  *http.Client
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
}

// Client is safe for concurrent use. Writes to one table are sent one at a
// time, including their conflict retries.
type Client struct {
	endpoint  string
	appID     string
	appSecret string
	http      *http.Client
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time

	tablesMu sync.Mutex
	tables   map[string]*sync.Mutex
}

// APIError is a non-zero Feishu business code. The service message is not
// retained; MissingScopes lists the scopes an app must apply for.
type APIError struct {
	Code          int
	MissingScopes []string
}

func (e *APIError) Error() string {
	if len(e.MissingScopes) > 0 {
		return fmt.Sprintf("feishu API code %d: missing scopes %s", e.Code, strings.Join(e.MissingScopes, ", "))
	}
	return fmt.Sprintf("feishu API code %d", e.Code)
}

func New(o Options) (*Client, error) {
	if o.Endpoint == "" {
		o.Endpoint = "https://open.feishu.cn"
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, infra.Classify(errors.New("invalid Feishu endpoint"))
	}
	if strings.TrimSpace(o.AppID) == "" || strings.TrimSpace(o.AppSecret) == "" {
		return nil, &infra.Global{Dep: Dep, Reason: infra.CredentialsMissing, Err: errors.New("feishu app credentials missing")}
	}
	timeout := o.HTTPTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	hc := http.Client{Timeout: timeout, Transport: infra.HTTPTransport}
	if o.HTTPClient != nil {
		hc = *o.HTTPClient
	}
	// Redirects must never forward the tenant token.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{
		endpoint: strings.TrimRight(o.Endpoint, "/"), appID: o.AppID, appSecret: o.AppSecret,
		http: &hc, now: o.Now, sleep: o.Sleep, tables: make(map[string]*sync.Mutex),
	}, nil
}

// safeError retains classification without exposing transport URLs or bodies.
func safeError(err error) error {
	if errors.Is(err, context.Canceled) {
		return infra.Classify(context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return infra.Classify(context.DeadlineExceeded)
	}
	var transient *infra.Transient
	if errors.As(infra.Classify(err), &transient) {
		return &infra.Transient{Err: errors.New("feishu transport failed")}
	}
	return infra.Classify(errors.New("feishu request failed"))
}

func (c *Client) tenantToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && c.now().Add(tokenRefreshMargin).Before(c.tokenExpiry) {
		return c.token, nil
	}
	input := map[string]string{"app_id": c.appID, "app_secret": c.appSecret}
	var out struct {
		Code   int    `json:"code"`
		Token  string `json:"tenant_access_token"`
		Expire int64  `json:"expire"`
	}
	if err := c.send(ctx, "/open-apis/auth/v3/tenant_access_token/internal", http.MethodPost, "", input, &out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.Token == "" || out.Expire <= 0 {
		return "", &infra.Global{Dep: Dep, Reason: infra.AuthenticationFailed, Err: errors.New("feishu authentication failed")}
	}
	c.token, c.tokenExpiry = out.Token, c.now().Add(time.Duration(out.Expire)*time.Second)
	return c.token, nil
}

// call sends an authenticated request and decodes the envelope's data.
func (c *Client) call(ctx context.Context, method, path string, input, data any) error {
	token, err := c.tenantToken(ctx)
	if err != nil {
		return err
	}
	var out struct {
		Code  int             `json:"code"`
		Data  json.RawMessage `json:"data"`
		Error struct {
			PermissionViolations []struct {
				Subject string `json:"subject"`
			} `json:"permission_violations"`
		} `json:"error"`
	}
	if err := c.send(ctx, path, method, token, input, &out); err != nil {
		return err
	}
	switch out.Code {
	case 0:
	case CodeMissingScope, CodeNoPermission:
		apiErr := &APIError{Code: out.Code}
		for _, v := range out.Error.PermissionViolations {
			apiErr.MissingScopes = append(apiErr.MissingScopes, v.Subject)
		}
		return &infra.Global{Dep: Dep, Reason: infra.PermissionDenied, Err: apiErr}
	case CodeWriteConflict:
		return &infra.Transient{Err: &APIError{Code: out.Code}}
	default:
		return &infra.Ticket{Err: &APIError{Code: out.Code}}
	}
	if data == nil {
		return nil
	}
	if err := json.Unmarshal(out.Data, data); err != nil {
		return safeError(err)
	}
	return nil
}

// send decodes any JSON envelope regardless of HTTP status, because Feishu
// reports business codes such as missing scopes with 4xx statuses.
func (c *Client) send(ctx context.Context, path, method, token string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var payload []byte
	if input != nil {
		var err error
		if payload, err = json.Marshal(input); err != nil {
			return safeError(err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return safeError(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// No response, as with a timeout: the request may have been applied.
		// This includes a connection the HTTP/2 health check closed mid-request.
		return safeError(&infra.Transient{Err: err})
	}
	defer func() { _ = resp.Body.Close() }() // Read-only response; read errors are handled below.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if err != nil {
		return safeError(err)
	}
	if len(body) > 16<<20 {
		return infra.Classify(errors.New("feishu response too large"))
	}
	if err := json.Unmarshal(body, output); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return infra.Classify(infra.HTTPError{StatusCode: resp.StatusCode})
		}
		return safeError(err)
	}
	return nil
}

func (c *Client) tableLock(base, table string) *sync.Mutex {
	c.tablesMu.Lock()
	defer c.tablesMu.Unlock()
	key := base + "/" + table
	mu, ok := c.tables[key]
	if !ok {
		mu = &sync.Mutex{}
		c.tables[key] = mu
	}
	return mu
}

// write serialises writes per table and retries only write conflicts, which
// the service rejects before applying. Any other transient failure leaves the
// outcome unknown, so it is not replayed.
func (c *Client) write(ctx context.Context, base, table, action string, input, data any) error {
	mu := c.tableLock(base, table)
	mu.Lock()
	defer mu.Unlock()
	return infra.Retry(ctx, infra.Options{Idempotent: true, Sleep: c.sleep}, func(ctx context.Context) error {
		err := c.call(ctx, http.MethodPost, recordsPath(base, table)+"/"+action, input, data)
		var apiErr *APIError
		var transient *infra.Transient
		if errors.As(err, &transient) && (!errors.As(err, &apiErr) || apiErr.Code != CodeWriteConflict) {
			return &infra.Ticket{Err: infra.ErrWriteUnconfirmed}
		}
		return err
	})
}

func tablePath(base, table string) string {
	return "/open-apis/base/v3/bases/" + url.PathEscape(base) + "/tables/" + url.PathEscape(table)
}

func recordsPath(base, table string) string {
	return tablePath(base, table) + "/records"
}
