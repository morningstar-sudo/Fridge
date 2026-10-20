package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"fridge/internal/store"
)

// Scheduler runs enabled backup schedules via cron.
type Scheduler struct {
	a  *App
	mu sync.Mutex
	c  *cron.Cron
}

func NewScheduler(a *App) *Scheduler {
	return &Scheduler{a: a, c: cron.New(cron.WithLocation(time.Local))}
}

// Reload replaces all cron entries with one per enabled schedule.
func (s *Scheduler) Reload(ctx context.Context) error {
	scs, err := s.a.Store.ListEnabledSchedules(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.c.Entries() {
		s.c.Remove(e.ID)
	}
	for _, sc := range scs {
		if _, err := s.c.AddFunc(sc.Cron, func() { s.run(sc) }); err != nil {
			log.Printf("schedule %d: bad cron %q: %v", sc.ID, sc.Cron, err)
		}
	}
	return nil
}

func (s *Scheduler) run(sc store.Schedule) {
	_, err := s.a.StartBackup(context.Background(), sc.DatabaseID, sc.DestinationID, &sc.ID, "schedule")
	switch {
	case errors.Is(err, store.ErrConflict):
		log.Printf("schedule %d: skipped, backup running", sc.ID)
	case err != nil:
		log.Printf("schedule %d: %v", sc.ID, err)
		if s.a.Alert != nil {
			s.a.Alert(context.Background(), fmt.Sprintf("backup failed to start: schedule %d: %v", sc.ID, err))
		}
	}
}

func (s *Scheduler) Start() { s.c.Start() }
func (s *Scheduler) Stop()  { <-s.c.Stop().Done() }
