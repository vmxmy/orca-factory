package infra

import (
	"net/http"
	"time"
)

// HTTPTransport is the connection pool the Feishu and GitHub clients share.
// Behind HTTPS_PROXY an HTTP/2 connection can go silent while the local socket
// stays established, and a timed-out request only resets its stream, so later
// requests keep reusing the dead connection (OF-295). Pinging after 15s
// without a frame and closing 5s after an unanswered ping drops such a
// connection within 20s, inside one 30s request timeout.
var HTTPTransport http.RoundTripper = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 5 * time.Second}
	return t
}()
