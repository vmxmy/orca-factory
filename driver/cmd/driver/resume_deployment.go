package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func init() { Register("resume-deployment", resumeDeployment) }
func resumeDeployment(args []string) int {
	return runDeploymentRecovery(args, os.Stderr, func(ctx context.Context, issue, queue, sha, action, operator, reason string, dry bool) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		// Stop serve first. The shared lock prevents an operator decision from racing
		// a smoke intent or overwriting a newer progress receipt. Dry-run only reads.
		if !dry {
			lock, err := scheduler.Acquire(filepath.Join(home, ".local", "state", "orca-factory", "driver-serve.lock"))
			if err != nil {
				return fmt.Errorf("stop driver serve before deployment recovery: %w", err)
			}
			defer lock.Close()
		}
		cfg, err := config.Load(home)
		if err != nil {
			return err
		}
		runtime, err := scheduler.NewRuntime(ctx, cfg)
		if err != nil {
			return err
		}
		return runtime.ResumeDeployment(ctx, issue, queue, sha, action, operator, reason, dry)
	})
}
func runDeploymentRecovery(args []string, out io.Writer, apply func(context.Context, string, string, string, string, string, string, bool) error) int {
	fs := flag.NewFlagSet("resume-deployment", flag.ContinueOnError)
	fs.SetOutput(out)
	issue := fs.String("issue", "", "Linear identifier")
	queue := fs.String("queue", "", "frozen queue ID")
	sha := fs.String("sha", "", "actual squash SHA")
	action := fs.String("action", "", "retry-workflow, retry-smoke or confirm-passed")
	operator := fs.String("operator", "", "human confirming recovery")
	reason := fs.String("reason", "", "reconciliation evidence and reason")
	dry := fs.Bool("dry-run", false, "validate without writes or smoke")
	if fs.Parse(args) != nil {
		return 2
	}
	validSHA := len(*sha) == 40 && strings.Trim(*sha, "0123456789abcdefABCDEF") == ""
	if fs.NArg() != 0 || *issue == "" || *queue == "" || !validSHA || strings.TrimSpace(*operator) == "" || strings.TrimSpace(*reason) == "" || strings.ContainsAny(*operator+*reason, "\r\n") || (*action != "retry-workflow" && *action != "retry-smoke" && *action != "confirm-passed") {
		fmt.Fprintln(out, "Usage: driver resume-deployment --issue ID --queue QUEUE --sha SQUASH_SHA --action retry-workflow|retry-smoke|confirm-passed --operator NAME --reason RECONCILIATION [--dry-run]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := apply(ctx, *issue, *queue, *sha, *action, *operator, *reason, *dry); err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	if *dry {
		fmt.Fprintln(out, "Recovery validated; no changes made")
	} else {
		fmt.Fprintln(out, "Human recovery recorded; restart driver serve")
	}
	return 0
}
