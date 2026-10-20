package app

import "context"

// Scheduler is a stub; Task 9 replaces it.
type Scheduler struct{}

func (s *Scheduler) Reload(ctx context.Context) error { return nil }
