package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
)

func (e *Executor) pullRequestBody(ctx context.Context, t Ticket) (string, error) {
	client, ok := e.Linear.(*issues.GitHub)
	if !ok {
		return "Fixes " + t.Identifier + "\n\n" + t.Todo, nil
	}
	task, err := client.Read(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if task.URL == "" || task.ID != t.ID || task.Identifier != t.Identifier {
		return "", errors.New("GitHub issue reference unavailable")
	}
	return fmt.Sprintf("Fixes %s\n\nCanonical identifier: %s\nCanonical ID: %s\n\n%s", task.URL, t.Identifier, t.ID, t.Todo), nil
}
