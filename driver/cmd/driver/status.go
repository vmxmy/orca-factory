package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func init() { Register("status", showStatus) }

func showStatus(args []string) int {
	asJSON, fresh := false, false
	repo := ""
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--json":
			asJSON = true
		case "--fresh":
			fresh = true
		case "--repo":
			index++
			if index >= len(args) {
				return statusUsage()
			}
			repo = args[index]
		default:
			return statusUsage()
		}
	}
	if fresh != (repo != "") {
		return statusUsage()
	}
	view := status.Read(time.Now(), 0)
	code := 0
	if fresh {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var err error
		view, err = freshStatus(ctx, repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			view = status.View{State: "unknown", Tickets: []status.Ticket{}}
			code = 1
		}
	}
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(view); err != nil {
			fmt.Fprintln(os.Stderr, "cannot output driver status")
			return 1
		}
	} else {
		fmt.Printf("Driver %s · tickets %d\n", view.State, len(view.Tickets))
		fmt.Printf("GitHub requests %d\n", view.GitHub.Requests)
		for _, t := range view.Tickets {
			if t.BlockedReason != "" {
				fmt.Printf("Blocked %s reason=%q\n", t.Identifier, t.BlockedReason)
			}
			if d := t.Deployment; d != nil {
				fmt.Printf("Deployment %s queue=%q sha=%s state=%s reason=%q\n", t.Identifier, d.QueueID, d.SHA, d.State, d.Reason)
			}
		}
	}
	return code
}

func statusUsage() int {
	fmt.Fprintln(os.Stderr, "Usage: orca-factory-driver status [--json] | status --fresh --repo <path> [--json]")
	return 2
}

func freshStatus(ctx context.Context, path string) (status.View, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return status.View{}, err
	}
	repo, err := config.LoadRepository(path)
	if err != nil {
		return status.View{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return status.View{}, err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return status.View{}, err
	}
	reader, err := issues.New(ctx, repo.Env, cfg.Tuning)
	if err != nil {
		return status.View{}, err
	}
	return freshStatusSnapshot(ctx, repo, reader)
}

func freshStatusSnapshot(ctx context.Context, repo config.Repository, reader issues.Store) (status.View, error) {
	runtime := &scheduler.Runtime{Clients: map[string]issues.Store{repo.Path: reader}}
	tickets, err := runtime.Snapshot(ctx, []config.Repository{repo})
	if err != nil {
		return status.View{}, err
	}
	scheduler.ObserveScopeWaits(ctx, tickets, []config.Repository{repo}, runtime)
	now := time.Now()
	return status.View{State: "ok", GeneratedAt: now, Tickets: scheduler.ObservationRows(tickets, []config.Repository{repo}, now)}, nil
}
