package github

import (
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"net/http"
	"strconv"
	"time"
)

type Usage struct {
	Hour                       time.Time
	Requests, Limit, Remaining int64
	Reset                      time.Time
}

func (c *Client) rateReady() error {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	if c.now().Before(c.blockedUntil) {
		return &infra.Global{Dep: infra.GitHub, Err: errors.New("GitHub quota pause active")}
	}
	return nil
}
func (c *Client) recordUsage(resp *http.Response) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	now := c.now()
	hour := now.UTC().Truncate(time.Hour)
	if !hour.Equal(c.usage.Hour) {
		c.usage.Hour = hour
		c.usage.Requests = 0
	}
	c.usage.Requests++
	if resp == nil {
		return
	}
	h := resp.Header
	if n, err := strconv.ParseInt(h.Get("X-RateLimit-Limit"), 10, 64); err == nil {
		c.usage.Limit = n
	}
	if n, err := strconv.ParseInt(h.Get("X-RateLimit-Remaining"), 10, 64); err == nil {
		c.usage.Remaining = n
	}
	if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		c.usage.Reset = time.Unix(n, 0)
	}
	if h.Get("X-RateLimit-Remaining") == "0" && c.usage.Reset.After(c.blockedUntil) {
		c.blockedUntil = c.usage.Reset
	}
	if resp.StatusCode == 429 || resp.StatusCode == 403 {
		until := now.Add(time.Minute)
		if n, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && n > 0 && n < 86400 {
			until = now.Add(time.Duration(n) * time.Second)
		} else if date, err := http.ParseTime(h.Get("Retry-After")); err == nil && date.After(now) {
			until = date
		}
		if until.After(c.blockedUntil) {
			c.blockedUntil = until
		}
	}
}
func (c *Client) Usage() Usage { c.quotaMu.Lock(); defer c.quotaMu.Unlock(); return c.usage }
