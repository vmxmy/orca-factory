package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitForWakeFileChange(t *testing.T) {
	for _, change := range []string{"modified", "older-mtime", "created"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "scheduler-wake.json")
				initial := time.Unix(100, 0)
				if change != "created" {
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, initial, initial); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- waitForWake(ctx, root, 30*time.Second) }()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("returned before file changed: %v", err)
				default:
				}
				time.Sleep(time.Second)
				if change == "created" {
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				modified := initial.Add(time.Second)
				if change == "older-mtime" {
					modified = initial.Add(-time.Second)
				}
				if err := os.Chtimes(path, modified, modified); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("wait did not return within 3 seconds of the file change")
				}
			})
		})
	}
}

func TestWaitForWakeTimeout(t *testing.T) {
	for _, exists := range []bool{false, true} {
		for _, duration := range []time.Duration{250 * time.Millisecond, 5 * time.Second} {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				if exists {
					if err := os.WriteFile(filepath.Join(root, "scheduler-wake.json"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				start := time.Now()
				if err := waitForWake(context.Background(), root, duration); err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(start); elapsed != duration {
					t.Fatalf("exists=%v: waited %v, want %v", exists, elapsed, duration)
				}
			})
		}
	}
}

func TestWaitForWakeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		root := t.TempDir()
		done := make(chan error, 1)
		go func() { done <- waitForWake(ctx, root, 30*time.Second) }()
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
		default:
			t.Fatal("wait did not return immediately on cancellation")
		}
	})
}
