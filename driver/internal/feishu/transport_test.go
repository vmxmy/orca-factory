package feishu

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

// connectProxy is an HTTP CONNECT proxy whose open tunnels can be frozen:
// a frozen tunnel stays open in both directions but forwards nothing, like a
// proxy whose upstream died without closing the client side. Tunnels opened
// after a freeze forward normally.
type connectProxy struct {
	ln     net.Listener
	opened atomic.Int64
	stop   chan struct{}

	mu      sync.Mutex
	conns   []net.Conn
	tunnels []*atomic.Bool
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{ln: ln, stop: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { p.tunnel(client) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		close(p.stop)
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		wg.Wait()
	})
	return p
}

func (p *connectProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.stop:
		_ = c.Close()
		return false
	default:
		p.conns = append(p.conns, c)
		return true
	}
}

func (p *connectProxy) tunnel(client net.Conn) {
	if !p.track(client) {
		return
	}
	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	upstream, err := net.Dial("tcp", req.Host)
	if err != nil || !p.track(upstream) {
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	frozen := &atomic.Bool{}
	p.mu.Lock()
	p.tunnels = append(p.tunnels, frozen)
	p.mu.Unlock()
	p.opened.Add(1)
	go p.forward(upstream, br, frozen)
	p.forward(client, upstream, frozen)
}

func (p *connectProxy) forward(dst io.Writer, src io.Reader, frozen *atomic.Bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && frozen.Load() {
			<-p.stop
			return
		}
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *connectProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, frozen := range p.tunnels {
		frozen.Store(true)
	}
}

// TestRequestsRecoverAfterProxyTunnelFreezes reproduces OF-295: an HTTP/2
// connection through HTTPS_PROXY went silent while staying established, and
// every later ledger read timed out on it until the service restarted.
func TestRequestsRecoverAfterProxyTunnelFreezes(t *testing.T) {
	var http2Requests atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			http2Requests.Add(1)
		}
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"t-token","expire":7200}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"tables":[{"id":"tbl1","name":"票据"}]}}`)
	}))
	s.EnableHTTP2 = true
	s.StartTLS()
	t.Cleanup(s.Close)
	proxy := newConnectProxy(t)

	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	transport := infra.HTTPTransport.(*http.Transport).Clone()
	// send gives each request 30s; production must drop a dead connection within one.
	// A zero PingTimeout means net/http's default of 15s.
	h := transport.HTTP2
	if h == nil || h.SendPingTimeout <= 0 {
		t.Fatalf("production HTTP/2 health check = %+v, want pings enabled", h)
	}
	pingTimeout := h.PingTimeout
	if pingTimeout == 0 {
		pingTimeout = 15 * time.Second
	}
	if h.SendPingTimeout+pingTimeout >= 30*time.Second {
		t.Fatalf("production HTTP/2 health check drops a dead connection after %s, want under 30s", h.SendPingTimeout+pingTimeout)
	}
	transport.HTTP2.SendPingTimeout, transport.HTTP2.PingTimeout = 100*time.Millisecond, 100*time.Millisecond
	transport.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: proxy.ln.Addr().String()})
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	t.Cleanup(transport.CloseIdleConnections)
	c, err := New(Options{Endpoint: s.URL, AppID: testAppID, AppSecret: testAppSecret, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}

	const requestTimeout = time.Second
	tables := func() (time.Duration, error) {
		ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
		defer cancel()
		started := time.Now()
		_, err := c.Tables(ctx, testBase)
		return time.Since(started), err
	}
	if _, err := tables(); err != nil {
		t.Fatal(err)
	}
	if http2Requests.Load() != 2 || proxy.opened.Load() != 1 {
		t.Fatalf("setup: %d HTTP/2 requests over %d tunnels, want 2 over 1", http2Requests.Load(), proxy.opened.Load())
	}

	proxy.freeze()
	firstTook, firstErr := tables()
	var transient *infra.Transient
	if !errors.As(firstErr, &transient) {
		t.Fatalf("first request after freeze: %v after %v; want a transient failure", firstErr, firstTook)
	}
	nextTook, nextErr := tables()
	if nextErr != nil || proxy.opened.Load() != 2 {
		t.Fatalf("next request: %v after %v over %d tunnels; want success over a second tunnel", nextErr, nextTook, proxy.opened.Load())
	}
}
