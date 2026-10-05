package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

// dryBackend performs every read for real and prints, instead of performing,
// every write the scheduler would make in this tick.
type dryBackend struct {
	scheduler.Backend
	out io.Writer
}

func (b *dryBackend) Snapshot(ctx context.Context, repos []config.Repository) ([]scheduler.Ticket, error) {
	tickets, err := b.Backend.Snapshot(ctx, repos)
	for _, t := range tickets {
		note := ""
		if p := t.Progress; p != nil && p.Deployment != nil {
			fmt.Fprintf(b.out, "deployment %s queue=%q sha=%q workflow=%q state=%q reason=%q\n", t.Identifier, p.QueueID, p.MergeSHA, p.Deployment.Workflow, p.Deployment.State, p.Deployment.Reason)
		}
		if t.Blocked {
			note = " blocked"
		}
		fmt.Fprintf(b.out, "ticket %s%s owner=%q phase=%q candidate=%v term=%q run=%q attempt=%d updated=%s\n", t.Identifier, note, t.Owner, t.Phase, t.Candidate, t.Terminal, t.Run, t.Attempt, t.UpdatedAt.Format(time.RFC3339))
	}
	return tickets, err
}
func (b *dryBackend) Apply(_ context.Context, t scheduler.Ticket, a scheduler.Action) error {
	fmt.Fprintf(b.out, "would %s %s attempt=%d phase=%q reason=%q\n", a.Kind, t.Identifier, a.Attempt, a.Phase, a.Reason)
	return nil
}
func (b *dryBackend) Launch(_ context.Context, t scheduler.Ticket) error {
	fmt.Fprintf(b.out, "would launch %s\n", t.Identifier)
	return nil
}
func (b *dryBackend) LinearLimits() []linear.RateLimit {
	if source, ok := b.Backend.(interface{ LinearLimits() []linear.RateLimit }); ok {
		return source.LinearLimits()
	}
	return nil
}

// Runtime.ActualChanges fetches remote refs. Dry-run uses declared scope.
func (b *dryBackend) ActualChanges(context.Context, scheduler.Ticket) ([]string, bool, error) {
	return nil, false, nil
}

// dryRunTick runs one scheduler tick without the serve lock or pause manager, so
// it neither competes with the running service nor writes probes or alerts.
func dryRunTick(ctx context.Context, cfg config.Config, backend scheduler.Backend, out io.Writer, now func() time.Time) error {
	s := &scheduler.Scheduler{Config: cfg, Backend: &dryBackend{Backend: backend, out: out}, Now: now}
	return s.Tick(ctx)
}

// Legacy branch verification fetches objects. Do not simulate a release or
// restart from missing evidence when dry-run deliberately omits that mutation.
func (b *dryBackend) InspectLegacy(_ context.Context, t scheduler.Ticket) (scheduler.Legacy, error) {
	fmt.Fprintf(b.out, "would reconcile legacy %s (git fetch omitted in dry-run)\n", t.Identifier)
	return scheduler.Legacy{}, errors.New("legacy verification requires git fetch")
}
