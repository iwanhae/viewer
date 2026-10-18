package admin

import (
	"context"

	"viewer/internal/catalog"
	"viewer/internal/runs"
)

func (s *Service) WithRunManager(manager *runs.Manager) *Service { s.runs = manager; return s }

func (s *Service) CreateRun(ctx context.Context, actor, stage, key string) (catalog.JobRun, bool, error) {
	if err := catalog.ValidateJobRunRequest(stage, key); err != nil {
		return catalog.JobRun{}, false, err
	}
	if s.runs == nil {
		return catalog.JobRun{}, false, runs.ErrUnavailable
	}
	return s.runs.Create(ctx, actor, stage, key)
}
func (s *Service) CancelRun(ctx context.Context, id string) (catalog.JobRun, error) {
	if s.runs == nil {
		return catalog.JobRun{}, runs.ErrUnavailable
	}
	return s.runs.Cancel(ctx, id)
}
func (s *Service) Runs(ctx context.Context, limit int, cursor string) (catalog.JobRunPage, error) {
	return s.cat.JobRuns(ctx, limit, cursor)
}
func (s *Service) Run(ctx context.Context, id string, limit int, cursor string) (catalog.JobRunItemPage, error) {
	return s.cat.JobRunItems(ctx, id, limit, cursor)
}
