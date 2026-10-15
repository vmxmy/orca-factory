package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TicketContext is one ledger ticket the project's in-flight ticket list
// shows the Agent before every turn (OF-33): an identifier, its ledger
// state, its title, and the identifiers of the tickets it depends on.
type TicketContext struct {
	Identifier, State, Title string
	Blockers                 []string
}

// ticketContextPath is where a project checkout carries its ticket list,
// relative to the checkout root. excludeIntakeDir keeps it out of git; it is
// never committed.
const ticketContextPath = ".intake/tickets.md"

var closedTicketStates = map[string]bool{"Done": true, "Canceled": true, "Cancelled": true, "Duplicate": true}

// staleTicketMarker matches a previous writeTicketContext staleness note so
// a repeated ledger failure updates its timestamp instead of stacking notes.
var staleTicketMarker = regexp.MustCompile(`^账本读取失败，内容可能过期（[^\n]*）\n`)

// renderTicketContext is .intake/tickets.md's content: one line per open
// ticket (Done/Canceled/Duplicate tickets never show), each listing its
// identifier, state, title and the tickets it depends on.
func renderTicketContext(tickets []TicketContext) string {
	var b strings.Builder
	b.WriteString("本项目的在途票（关闭的 Done/Canceled/Duplicate 不列出）：\n")
	open := 0
	for _, t := range tickets {
		if closedTicketStates[t.State] {
			continue
		}
		open++
		deps := "无"
		if len(t.Blockers) > 0 {
			deps = strings.Join(t.Blockers, ", ")
		}
		fmt.Fprintf(&b, "- %s [%s] %s（依赖：%s）\n", t.Identifier, t.State, t.Title, deps)
	}
	if open == 0 {
		b.WriteString("（当前没有在途票）\n")
	}
	return b.String()
}

// writeTicketContext refreshes the checkout's .intake/tickets.md before
// every Agent turn (OF-33): the read-only Agent has no other way to see
// which tickets already exist. r.Tickets is nil for a gateway with no
// projects configured (pre-OF-32), which writes nothing. A ledger read
// failure keeps the previous file and prefixes a staleness note rather than
// blocking the turn.
func (r *Runner) writeTicketContext(ctx context.Context) {
	if r.Tickets == nil {
		return
	}
	path := filepath.Join(r.Checkout, ticketContextPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if tickets, err := r.Tickets(ctx); err == nil {
		_ = writeAtomic(path, []byte(renderTicketContext(tickets)))
		return
	}
	prev, _ := os.ReadFile(path)
	prev = staleTicketMarker.ReplaceAll(prev, nil)
	marker := fmt.Sprintf("账本读取失败，内容可能过期（%s）\n", time.Now().Format("2006-01-02 15:04:05"))
	_ = writeAtomic(path, append([]byte(marker), prev...))
}
