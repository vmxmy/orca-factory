package issues

import (
	"context"
	"errors"

	"github.com/vmxmy/orca-factory/driver/internal/ledger"
)

// LedgerStore uses the same repository and logical comment history as tasks.
type LedgerStore struct{ Client Store }

func (s *LedgerStore) ListIssues(ctx context.Context) ([]ledger.Issue, error) {
	rows, err := s.Client.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []ledger.Issue{}
	for _, r := range rows {
		if _, ok := ledger.Number(r.Title); ok {
			out = append(out, ledger.Issue{ID: r.ID, Title: r.Title, State: r.State.Name, UpdatedAt: r.UpdatedAt})
		}
	}
	return out, nil
}
func (s *LedgerStore) CreateIssue(ctx context.Context, title, state string) (ledger.Issue, error) {
	if _, ok := ledger.Number(title); !ok || !validState(state) {
		return ledger.Issue{}, errors.New("invalid ledger title or state")
	}
	r, err := s.Client.Create(ctx, CreateInput{Title: title, Labels: []string{}})
	if err != nil {
		return ledger.Issue{}, err
	}
	m := r.Metadata
	m.Complete = true
	m.State = state
	r, err = s.Client.SetMetadata(ctx, r.ID, m)
	if err != nil {
		return ledger.Issue{}, err
	}
	if stateType(state) == "completed" || stateType(state) == "canceled" || stateType(state) == "duplicate" {
		if _, err = s.Client.UpdateIssue(ctx, r.ID, state, nil, nil); err != nil {
			return ledger.Issue{}, err
		}
	}
	return ledger.Issue{ID: r.ID, Title: r.Title, State: r.State.Name, UpdatedAt: r.UpdatedAt}, nil
}
func (s *LedgerStore) SetState(ctx context.Context, id, state string) error {
	_, err := s.Client.UpdateIssue(ctx, id, state, nil, nil)
	return err
}
func (s *LedgerStore) CreateComment(ctx context.Context, id, body string) error {
	_, err := s.Client.CreateComment(ctx, id, body)
	return err
}
func (s *LedgerStore) ListComments(ctx context.Context, id string) ([]ledger.Comment, error) {
	rows, err := s.Client.ListComments(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []ledger.Comment{}
	for _, r := range rows {
		out = append(out, ledger.Comment{Body: r.Body, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}
