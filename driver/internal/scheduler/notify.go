package scheduler

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/intake"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// NotifyFeishu reserves an event before sending. A failed or uncertain send
// keeps the reservation, including across driver restarts.
func (r *Runtime) NotifyFeishu(ctx context.Context, t Ticket) error {
	env := r.Repositories[t.Repo].Env
	if env["AGENT_ISSUE_BACKEND"] != "feishu" {
		return nil
	}
	category, reason := feishuNotification(t)
	if category == "" {
		return nil
	}
	human := category == "转人" && t.Resolution != nil && t.Resolution.Phase == "human"
	reason = strings.Join(strings.Fields(reason), " ")
	identity := reason
	if human {
		identity = fmt.Sprintf("%s attempt=%d", reason, t.Resolution.Attempt)
	}
	if category == "搁置" {
		identity = t.ID
	}
	digest := sha256.Sum256([]byte(identity))
	marker := fmt.Sprintf("通知: %s %x", category, digest[:6])
	// A notified request already had its auto review; skip the ledger reread.
	if t.NotifyEvents[marker] {
		return nil
	}
	var refusals []string
	// Auto mode never hands a ticket to a human, so a human ticket here predates
	// the switch and keeps its manual handling: only explicit flags apply.
	enabled := func(name string) bool {
		return human && !t.Resolution.TopUsed && !t.Resolution.Top && explicitAutoReview(env, name)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if enabled("scope") {
		refusal, err := intake.AutoApproveScope(ctx, r.Clients[t.Repo], t.ID, t.Resolution.Attempt, env["AGENT_LANE3_GLOBS"], now)
		if err != nil {
			return fmt.Errorf("auto scope review: %w", err)
		}
		if refusal == "" {
			return nil
		}
		refusals = append(refusals, "扩SCOPE："+refusal)
	}
	if enabled("release") {
		released, refusal, err := intake.AutoReleaseHuman(ctx, r.Clients[t.Repo], t.ID, t.Resolution.Attempt, reason, now)
		if err != nil {
			return fmt.Errorf("auto release review: %w", err)
		}
		if released || refusal == "" {
			return nil
		}
		refusals = append(refusals, "放行："+refusal)
	}
	autoRefusal := strings.Join(refusals, "；")
	settings, err := config.FeishuNotifySettings(env)
	if err != nil {
		return err
	}
	chat := strings.TrimSpace(settings["AGENT_FEISHU_NOTIFY_CHAT"])
	if chat == "" {
		return nil
	}
	client := r.Clients[t.Repo]
	sender, err := feishu.New(feishu.Options{Endpoint: settings["AGENT_FEISHU_ENDPOINT"], AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		return err
	}
	if _, err := client.CreateComment(ctx, t.ID, marker); err != nil {
		return fmt.Errorf("notification event write: %w", err)
	}
	if human {
		card := feishu.HumanCard{Ticket: t.Identifier, Title: t.Title, Reason: reason, URL: t.URL, Attempt: t.Resolution.Attempt, AutoReviewRefusal: autoRefusal}
		card.Scope = notificationScope(t)
		return sender.SendCard(ctx, chat, card.JSON())
	}
	body := fmt.Sprintf("%s %s\n类别: %s\n原因: %s\n%s", t.Identifier, t.Title, category, reason, t.URL)
	if category == "转人" {
		body += fmt.Sprintf("\n可用命令：@机器人 放行 %s 或 @机器人 取消 %s <理由>", t.Identifier, t.Identifier)
	}
	return sender.SendText(ctx, chat, body)
}

func feishuNotification(t Ticket) (string, string) {
	switch {
	case t.Parked != "":
		return "搁置", t.Parked
	case t.MergeProblem != "":
		return "证据冲突", t.MergeProblem
	case t.Progress != nil && t.Progress.Deployment != nil && t.Progress.Deployment.State == "paused":
		reason := t.Progress.Deployment.Reason
		if reason == "" {
			reason = t.NotifyReason
		}
		return "部署失败", reason
	case t.Owner == machine.OwnerHuman && (t.Resolution == nil || t.Resolution.Phase != "waiting"):
		return "转人", t.NotifyReason
	default:
		return "", ""
	}
}

func readFeishuNotification(t *Ticket, comments []task.Comment) {
	rows := append([]task.Comment(nil), comments...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	t.NotifyReason = ""
	t.NotifyEvents = map[string]bool{}
	for _, c := range rows {
		if strings.HasPrefix(c.Body, "通知: ") {
			t.NotifyEvents[c.Body] = true
		}
		for _, prefix := range []string{"转人: ", "Agent处置: "} {
			if strings.HasPrefix(c.Body, prefix) {
				t.NotifyReason = strings.SplitN(strings.TrimPrefix(c.Body, prefix), "\n", 2)[0]
			}
		}
	}
	if t.NotifyReason == "" && t.Progress != nil {
		t.NotifyReason = t.Progress.Reason
	}
	if t.NotifyReason == "" {
		t.NotifyReason = "等待处置"
	}
}

func notificationScope(t Ticket) []string {
	if t.Resolution == nil {
		return nil
	}
	decision, err := resolver.ParseResult(t.Resolution.Result)
	if err != nil {
		return nil
	}
	return decision.ScopeRequest()
}

func (r *Runtime) NotifyOrcaRuntime(ctx context.Context, ticket Ticket) error {
	return r.sendOrcaRuntimeText(ctx, ticket, "Orca 运行时故障：terminal create 连续返回 runtime_error，已暂停认领与派工，保留票据 attempt。暂停满 10 分钟后 driver 自动重启 orca-serve 服务，之后每 30 分钟最多一次、共 3 次；调度器探测成功后自动恢复。请查看 orca-serve 日志里的 profile 写入超时。")
}

// NotifyOrcaRestart reports one automatic orca-serve restart and its exit code.
func (r *Runtime) NotifyOrcaRestart(ctx context.Context, ticket Ticket, restart int, restartErr error) error {
	return r.sendOrcaRuntimeText(ctx, ticket, fmt.Sprintf("Orca 运行时故障：driver 第 %d/%d 次自动重启 orca-serve 服务，systemctl 退出码 %d；driver 将继续读取自动化状态或探测终端创建，确认恢复后继续调度。", restart, orcaRestartLimit, restartExitCode(restartErr)))
}

// restartExitCode is systemctl's exit code, or -1 when it did not exit on its own.
func restartExitCode(restartErr error) int {
	if restartErr == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(restartErr, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func (r *Runtime) NotifyOrcaRestartExhausted(ctx context.Context, ticket Ticket) error {
	return r.sendOrcaRuntimeText(ctx, ticket, fmt.Sprintf("Orca 自动重启 %d 次仍未恢复，需人工处理：Orca 仍不可达或 terminal create 探测仍失败，driver 不再重启 orca-serve，只继续读取自动化状态或探测。请查看 orca-serve 日志里的 profile 写入超时。", orcaRestartLimit))
}

// errNoticeNotSent marks a notice that failed before any request reached
// Feishu, so sending it again cannot duplicate it.
var errNoticeNotSent = errors.New("notice not sent")

func (r *Runtime) sendOrcaRuntimeText(ctx context.Context, ticket Ticket, text string) error {
	return sendNoticeText(ctx, r.Repositories[ticket.Repo].Env, text)
}

// sendNoticeText holds the maintenance write lock through the send, so a
// frozen driver sends nothing and a freeze waits for a send in flight.
func sendNoticeText(ctx context.Context, env map[string]string, text string) error {
	release, err := maintenance.Write()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	defer release()
	settings, err := config.FeishuNotifySettings(env)
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	chat := strings.TrimSpace(settings["AGENT_FEISHU_NOTIFY_CHAT"])
	if chat == "" {
		return nil
	}
	attempts, err := newMessageAttempts()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	defer attempts.transport.CloseIdleConnections()
	sender, err := feishu.New(feishu.Options{Endpoint: settings["AGENT_FEISHU_ENDPOINT"], AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"], HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: attempts}})
	if err != nil {
		return fmt.Errorf("%w: %w", errNoticeNotSent, err)
	}
	if err := sender.SendText(ctx, chat, text); err != nil {
		if !attempts.written() {
			return fmt.Errorf("%w: %w", errNoticeNotSent, err)
		}
		return err
	}
	return nil
}

// messageAttempts reports whether any byte of a message request reached a
// socket, so a failure before that, such as a tenant token timeout, a refused
// connection, a first write that sent nothing or a failed reconnect handshake,
// is known undelivered. It speaks HTTP/1.1 only, so after GotConn the
// connection carries nothing but the request until it closes.
type messageAttempts struct {
	transport *http.Transport
	dial      func(context.Context, string, string) (net.Conn, error)

	mu      sync.Mutex
	conns   []*countingConn
	unknown bool // a message got a connection this type did not dial
}

// messageWriteWait bounds the wait for a failed message connection to close;
// past it the attempt counts as possibly delivered.
const messageWriteWait = 5 * time.Second

func newMessageAttempts() (*messageAttempts, error) {
	base, ok := infra.HTTPTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("feishu transport is not an http.Transport")
	}
	m := &messageAttempts{transport: base.Clone(), dial: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
	var http1 http.Protocols
	http1.SetHTTP1(true)
	m.transport.Protocols = &http1
	// Protocols leaves ALPN alone; a cloned config may still offer h2.
	if m.transport.TLSClientConfig == nil {
		m.transport.TLSClientConfig = &tls.Config{}
	}
	m.transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	m.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := m.dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		counted := &countingConn{Conn: conn, closed: make(chan struct{})}
		counted.base.Store(-1)
		m.mu.Lock()
		m.conns = append(m.conns, counted)
		m.mu.Unlock()
		return counted, nil
	}
	return m, nil
}

func (m *messageAttempts) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/open-apis/im/v1/messages") {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: m.gotConn}))
	}
	return m.transport.RoundTrip(req)
}

// gotConn starts counting on the socket under every TLS layer, after any TLS
// handshake or proxy CONNECT.
func (m *messageAttempts) gotConn(info httptrace.GotConnInfo) {
	conn := info.Conn
	for {
		secure, ok := conn.(*tls.Conn)
		if !ok {
			break
		}
		conn = secure.NetConn()
	}
	if counted, ok := conn.(*countingConn); ok {
		counted.base.CompareAndSwap(-1, counted.sent.Load())
		return
	}
	m.mu.Lock()
	m.unknown = true
	m.mu.Unlock()
}

// written reports whether a message request may have reached Feishu. A failed
// request's connection closes, and an idle one is closed here, so it waits for
// that before trusting a zero count.
func (m *messageAttempts) written() bool {
	m.transport.CloseIdleConnections()
	m.mu.Lock()
	conns, unknown := slices.Clone(m.conns), m.unknown
	m.mu.Unlock()
	if unknown {
		return true
	}
	for _, conn := range conns {
		if conn.base.Load() < 0 {
			continue
		}
		select {
		case <-conn.closed:
		case <-time.After(messageWriteWait):
			return true
		}
		if conn.sent.Load() > conn.base.Load() {
			return true
		}
	}
	return false
}

// countingConn counts bytes the socket took; base is the count when a message
// request got this connection, -1 before that. The HTTP/1 transport sets no
// write deadline, but TLS close sets one before writing close_notify, so
// counting stops there.
type countingConn struct {
	net.Conn
	sent      atomic.Int64
	base      atomic.Int64
	closing   atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if !c.closing.Load() {
		c.sent.Add(int64(n))
	}
	return n, err
}

func (c *countingConn) SetWriteDeadline(t time.Time) error {
	c.closing.Store(true)
	return c.Conn.SetWriteDeadline(t)
}

func (c *countingConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { close(c.closed) })
	return err
}

// NotifyHostMemory uses the machine notification chat because the hold spans repositories.
func (r *Runtime) NotifyHostMemory(ctx context.Context, memory status.HostMemory) error {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	return sendNoticeText(ctx, nil, fmt.Sprintf("内存吃紧，已暂停认领与新建终端 %s：可用量 %d bytes，some avg60 %.2f，限制来源 %s；用 systemd-cgls 与该切片的 memory.peak 找占用大户，读数恢复后自动放开。", now.Sub(memory.Since).Round(time.Second), memory.Reading.Available, memory.Reading.SomeAvg60, memory.Reading.Source))
}
