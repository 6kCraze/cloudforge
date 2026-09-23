package jobs

import "context"

type JobStore interface {
	Create(context.Context, CreateInput) (Job, error)
	Get(context.Context, string) (Job, error)
}

type Service struct {
	store JobStore
}

func NewService(store JobStore) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (Job, error) {
	if err := ValidateCreate(input); err != nil {
		return Job{}, err
	}
	return s.store.Create(ctx, input)
}

func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	return s.store.Get(ctx, id)
}
