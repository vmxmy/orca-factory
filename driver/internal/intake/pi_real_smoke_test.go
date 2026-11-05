package intake

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 手动冒烟：PI_REAL_SMOKE=1 go test -run TestPiRealSmoke -v ./internal/intake
// 用真 pi 进程验证常驻路径端到端（会调真模型），不进 CI。
func TestPiRealSmoke(t *testing.T) {
	if os.Getenv("PI_REAL_SMOKE") != "1" {
		t.Skip("set PI_REAL_SMOKE=1")
	}
	bin, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("pi not on PATH")
	}
	dir := t.TempDir()
	r := &Runner{
		PiBinary:      bin,
		PiProvider:    "magpie",
		PiModel:       "codex/gpt-6-astra",
		PiSessionDir:  filepath.Join(dir, "sessions"),
		PiConfigDir:   filepath.Join(os.Getenv("HOME"), ".pi", "agent"),
		PiIdleTimeout: time.Minute,
		Timeout:       2 * time.Minute,
		Repo:          dir,
		Checkout:      dir,
	}
	r.Start()
	defer r.Stop()
	turn := Turn{Session: "smoke-session-1", Resume: true, Prompt: "只回复两个字：收到。不要输出其他内容。"}
	reply, err := r.Turn(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reply1: %q", reply)
	if !strings.Contains(reply, "收到") {
		t.Fatalf("reply lacks 收到: %q", reply)
	}
	turn2 := Turn{Session: "smoke-session-1", Resume: true, Prompt: "我刚才让你回复什么？只答那两个字。"}
	reply2, err := r.Turn(context.Background(), turn2)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reply2: %q", reply2)
	if !strings.Contains(reply2, "收到") {
		t.Fatalf("reply2 lacks context memory: %q", reply2)
	}
}
