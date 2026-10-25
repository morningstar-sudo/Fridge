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
	a   *App
	mu  sync.Mutex
	c   *cron.Cron
	ids []cron.EntryID // schedule entries from the last Reload; fixed jobs are never listed
}

func NewScheduler(a *App) *Scheduler {
	return &Scheduler{a: a, c: cron.New(cron.WithLocation(time.Local))}
}

// Reload replaces the schedule entries with one per enabled schedule; fixed jobs stay.
func (s *Scheduler) Reload(ctx context.Context) error {
	scs, err := s.a.Store.ListEnabledSchedules(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.ids {
		s.c.Remove(id)
	}
	s.ids = nil
	for _, sc := range scs {
		id, err := s.c.AddFunc(sc.Cron, func() { s.run(sc) })
		if err != nil {
			log.Printf("schedule %d: bad cron %q: %v", sc.ID, sc.Cron, err)
			continue
		}
		s.ids = append(s.ids, id)
	}
	return nil
}

// AddFixed registers a job that Reload never removes.
func (s *Scheduler) AddFixed(spec string, fn func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.c.AddFunc(spec, fn)
	return err
}

func (s *Scheduler) run(sc store.Schedule) {
	_, err := s.a.StartBackup(context.Background(), sc.DatabaseID, sc.DestinationID, &sc.ID, "schedule", nil)
	switch {
	case errors.Is(err, store.ErrConflict):
		log.Printf("schedule %d: skipped, backup running", sc.ID)
	case err != nil:
		log.Printf("schedule %d: %v", sc.ID, err)
		s.a.auditSystem(context.Background(), "system.backup", sc.DatabaseID, "schedule", sc.ID, false, map[string]any{"schedule_id": sc.ID, "started": false})
		if s.a.Alert != nil {
			s.a.Alert(context.Background(), fmt.Sprintf("backup failed to start: schedule %d: %v", sc.ID, err))
		}
	}
}

func (s *Scheduler) Start() { s.c.Start() }
func (s *Scheduler) Stop()  { <-s.c.Stop().Done() }
