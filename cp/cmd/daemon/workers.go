package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	database "jaiveer/ControlPlane/cp/internal/db"
	"jaiveer/ControlPlane/pkg/cluster"

	"gorm.io/gorm"
)

// workerStore persists worker liveness so the control plane can tell a live
// worker from one that has gone quiet, across restarts of either side.
//
// The database is only available once the daemon has been given a config, so
// every method is a no-op until setDB is called.
type workerStore struct {
	mu sync.RWMutex
	db *gorm.DB
}

func newWorkerStore() *workerStore {
	return &workerStore{}
}

func (s *workerStore) setDB(db *gorm.DB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
}

func (s *workerStore) handle() *gorm.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db
}

// find returns the stored row for a worker, or gorm.ErrRecordNotFound.
func (s *workerStore) find(name string) (*database.Workers, error) {
	db := s.handle()
	if db == nil {
		return nil, errors.New("the daemon has no config loaded yet")
	}
	var w database.Workers
	if err := db.Where("name = ?", name).First(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

// markOnline records that a worker has connected and stamps its last seen time.
func (s *workerStore) markOnline(name string) {
	db := s.handle()
	if db == nil {
		return
	}

	now := time.Now()
	var w database.Workers
	err := db.Where("name = ?", name).First(&w).Error
	switch {
	case err == nil:
		w.Status = cluster.StatusOnline
		w.LastSeen = now
		if err := db.Save(&w).Error; err != nil {
			slog.Error("Failed to update worker", "worker", name, "error", err)
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := db.Create(&database.Workers{
			Name:     name,
			Status:   cluster.StatusOnline,
			LastSeen: now,
		}).Error; err != nil {
			slog.Error("Failed to record worker", "worker", name, "error", err)
		}
	default:
		slog.Error("Failed to look up worker", "worker", name, "error", err)
	}
}

// touch updates a worker's last seen time on each heartbeat.
func (s *workerStore) touch(name string) {
	db := s.handle()
	if db == nil {
		return
	}

	// A single UPDATE avoids a read-modify-write on every heartbeat.
	err := db.Model(&database.Workers{}).
		Where("name = ?", name).
		Updates(map[string]any{
			"last_seen": time.Now(),
			"status":    cluster.StatusOnline,
		}).Error
	if err != nil {
		slog.Error("Failed to record heartbeat", "worker", name, "error", err)
	}
}

// markOffline flags a worker as offline. It is used both when a worker
// disconnects cleanly and when the sweeper decides one has gone silent.
func (s *workerStore) markOffline(name, reason string) {
	db := s.handle()
	if db == nil {
		return
	}

	err := db.Model(&database.Workers{}).
		Where("name = ?", name).
		Update("status", cluster.StatusOffline).Error
	if err != nil {
		slog.Error("Failed to mark worker offline", "worker", name, "error", err)
		return
	}
	slog.Warn("Worker marked offline", "worker", name, "reason", reason)
}

// silent returns the names of workers still marked online whose last seen time
// is older than the timeout.
func (s *workerStore) silent(timeout time.Duration) []string {
	db := s.handle()
	if db == nil {
		return nil
	}

	var workers []database.Workers
	err := db.Where("status = ? AND last_seen < ?", cluster.StatusOnline, time.Now().Add(-timeout)).
		Find(&workers).Error
	if err != nil {
		slog.Error("Failed to list silent workers", "error", err)
		return nil
	}

	names := make([]string, 0, len(workers))
	for _, w := range workers {
		names = append(names, w.Name)
	}
	return names
}

// list returns every known worker, ordered by name.
//
// Rows with an empty name are skipped: a worker always has a name (it comes from
// the bootstrap token), so those are leftovers from before the name column
// existed and are not real workers.
func (s *workerStore) list() []database.Workers {
	db := s.handle()
	if db == nil {
		return nil
	}

	var workers []database.Workers
	if err := db.Where("name <> ''").Order("name").Find(&workers).Error; err != nil {
		slog.Error("Failed to list workers", "error", err)
		return nil
	}
	return workers
}

// sweepSilentWorkers marks workers offline once they have missed
// cluster.OfflineAfter worth of heartbeats, and drops them from routing so
// deploys are no longer sent to a worker that has gone quiet.
//
// This is what catches a worker whose stream is still open but which has hung:
// a clean disconnect is handled directly by registerStream.
func (s *ControlPlane) sweepSilentWorkers() {
	for _, name := range s.WorkerStore.silent(cluster.OfflineAfter) {
		s.WorkerStore.markOffline(name, "missed heartbeats")

		// Take it out of the routing table so fan-out skips it, and close its
		// stream so the connection does not linger.
		if conn, ok := s.Workers.get(name); ok {
			slog.Warn("Dropping silent worker from routing", "worker", name)
			conn.shutdown()
		}
	}
}

// startWorkerSweeper runs sweepSilentWorkers on a ticker until ctx is done.
func (s *ControlPlane) startWorkerSweeper(ctx context.Context) {
	ticker := time.NewTicker(cluster.SweepInterval)
	defer ticker.Stop()

	slog.Info("Worker liveness sweeper started",
		"interval", cluster.SweepInterval, "offlineAfter", cluster.OfflineAfter)

	for {
		select {
		case <-ticker.C:
			s.sweepSilentWorkers()
		case <-ctx.Done():
			return
		}
	}
}

// listWorkers reports every known worker with its liveness, which makes the
// heartbeat bookkeeping observable from the CLI.
func (s *ControlPlane) listWorkers(w http.ResponseWriter, r *http.Request) {
	workers := s.WorkerStore.list()
	type row struct {
		Name      string    `json:"name"`
		Status    string    `json:"status"`
		LastSeen  time.Time `json:"last_seen"`
		AgeSec    float64   `json:"seconds_since_seen"`
		Connected bool      `json:"stream_connected"`
	}

	out := make([]row, 0, len(workers))
	for _, wk := range workers {
		_, connected := s.Workers.get(wk.Name)
		out = append(out, row{
			Name:      wk.Name,
			Status:    wk.Status,
			LastSeen:  wk.LastSeen,
			AgeSec:    time.Since(wk.LastSeen).Seconds(),
			Connected: connected,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
