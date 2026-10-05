// Package github implements the driver's GitHub REST operations.
package github

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
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Options injects transport, credentials, and timing. BaseURL defaults to
// https://api.github.com; an override is a trusted origin (usually httptest).
type Options struct {
	Logger     func(string) // Optional; nil disables logging.
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) error
}

type cachedResponse struct {
	etag string
	body []byte
}

// Client is safe for concurrent use. Each client owns a capacity-one write
// token bucket, refilled once per second, and an ETag cache.
type Client struct {
	logger     func(string)
	base       string
	http       *http.Client
	token      func(context.Context) (string, error)
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	writeMu    sync.Mutex
	lastWrite  time.Time
	wrote      bool
	cacheMu    sync.Mutex
	cache      map[string]cachedResponse
	generation uint64
	createMu   sync.Mutex
}

func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.github.com"
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, infra.Classify(errors.New("invalid GitHub base URL"))
	}
	hc := http.Client{Timeout: 30 * time.Second}
	if o.HTTPClient != nil {
		hc = *o.HTTPClient
	}
	// Redirects must never forward credentials, even between ports on one host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if o.Token == nil {
		o.Token = authToken
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	return &Client{logger: o.Logger, base: strings.TrimRight(o.BaseURL, "/"), http: &hc, token: o.Token, now: o.Now, sleep: o.Sleep, cache: make(map[string]cachedResponse)}, nil
}

func authToken(ctx context.Context) (string, error) {
	b, err := runx.Run(ctx, runx.Spec{Path: "gh", Args: []string{"auth", "token"}, Timeout: 10 * time.Second})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// safeError retains classification without exposing transport URLs, bodies, or
// errors supplied by a credential provider through an unwrap chain.
func safeError(err error) error {
	if errors.Is(err, context.Canceled) {
		return infra.Classify(context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return infra.Classify(context.DeadlineExceeded)
	}
	var transient *infra.Transient
	if errors.As(infra.Classify(err), &transient) {
		return &infra.Transient{Err: errors.New("GitHub transport failed")}
	}
	return infra.Classify(errors.New("GitHub request failed"))
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) (resultErr error) {
	started := time.Now()
	var resp *http.Response
	defer func() {
		if c.logger == nil {
			return
		}
		status, requestID := 0, ""
		if resp != nil {
			status = resp.StatusCode
			requestID = resp.Header.Get("X-GitHub-Request-Id")
		}
		endpoint, _, _ := strings.Cut(path, "?")
		result := "success"
		if resultErr != nil {
			result = "failure"
		}
		c.logger(fmt.Sprintf("github interface=%q status=%d result=%s duration_ms=%d request_id=%q", method+" "+runx.SafeText(endpoint), status, result, time.Since(started).Milliseconds(), runx.SafeText(requestID)))
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := c.token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return &infra.Global{Dep: infra.GitHub, Err: errors.New("GitHub authentication failed")}
	}
	var payload []byte
	if input != nil {
		payload, err = json.Marshal(input)
		if err != nil {
			return safeError(err)
		}
	}
	endpoint := c.base + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return safeError(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.cacheMu.Lock()
	cached, hasCache := c.cache[endpoint]
	generation := c.generation
	c.cacheMu.Unlock()
	if method == http.MethodGet && hasCache {
		req.Header.Set("If-None-Match", cached.etag)
	}
	if method != http.MethodGet {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		for c.wrote {
			remaining := time.Second - c.now().Sub(c.lastWrite)
			if remaining <= 0 {
				break
			}
			if err := c.sleep(ctx, remaining); err != nil {
				return safeError(err)
			}
		}
		if err := ctx.Err(); err != nil {
			return safeError(err)
		}
		c.lastWrite = c.now()
		c.wrote = true
		// Invalidate before and after even failed writes: the remote state may change.
		c.invalidate()
		defer c.invalidate()
	}
	resp, err = c.http.Do(req)
	if err != nil {
		return safeError(err)
	}
	defer resp.Body.Close()
	var body []byte
	if resp.StatusCode == http.StatusNotModified && method == http.MethodGet && hasCache {
		body = cached.body
	} else {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			cause := infra.HTTPError{StatusCode: resp.StatusCode}
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				return &infra.Global{Dep: infra.GitHub, Err: cause}
			}
			if resp.StatusCode == 429 {
				return &infra.Transient{Err: cause}
			}
			return infra.Classify(cause)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
		if err != nil {
			return safeError(err)
		}
		if len(body) > 16<<20 {
			return infra.Classify(errors.New("GitHub response too large"))
		}
		if method == http.MethodGet {
			c.cacheMu.Lock()
			if c.generation == generation {
				if etag := resp.Header.Get("ETag"); etag != "" {
					c.cache[endpoint] = cachedResponse{etag: etag, body: body}
				} else {
					delete(c.cache, endpoint)
				}
			}
			c.cacheMu.Unlock()
		}
	}
	if output != nil {
		if err = json.Unmarshal(body, output); err != nil {
			return safeError(err)
		}
	}
	return nil
}

func (c *Client) invalidate() {
	c.cacheMu.Lock()
	c.cache = make(map[string]cachedResponse)
	c.generation++
	c.cacheMu.Unlock()
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}
