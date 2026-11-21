package orca

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestHeartbeatFinishPreservesFinalBeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		path := filepath.Join(t.TempDir(), "heartbeat.json")
		h, err := StartHeartbeat(ctx, path, "term_exec", time.Second, nil)
		if err != nil {
			t.Fatal(err)
		}
		h.SetRun("run_one")
		if err := h.Finish(); err != nil {
			t.Fatal(err)
		}
		final, found, err := ReadBeat(path)
		if err != nil || !found || !final.Exited || final.Run != "run_one" || final.Terminal != "term_exec" || final.PID != os.Getpid() {
			t.Fatalf("final beat=%+v found=%v err=%v", final, found, err)
		}
		time.Sleep(2 * time.Second)
		h.SetRun("run_late")
		if err := h.beat(); err != nil {
			t.Fatal(err)
		}
		if err := h.Finish(); err != nil {
			t.Fatal(err)
		}
		got, found, err := ReadBeat(path)
		if err != nil || !found || got != final {
			t.Fatalf("final beat overwritten: got=%+v want=%+v err=%v", got, final, err)
		}
	})
}
