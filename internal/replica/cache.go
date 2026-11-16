package replica

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func MaintainCache(ctx context.Context, db *index.DB, c *journal.Coordinator, namespace, after string, limit int, remove bool) (journal.CacheReport, error) {
	bound, err := db.ReplicaNamespace()
	if err != nil {
		return journal.CacheReport{}, err
	}
	if bound != "" && bound != namespace {
		return journal.CacheReport{}, fmt.Errorf("index/replica namespace differs")
	}
	return c.MaintainReplicaCache(ctx, namespace, after, limit, remove, db.WithCachePins)
}

// Replica work and maintenance share the worker goroutine. Each turn freezes
// only a bounded set of path references, then yields to observation/transfers.
func (w *Worker) maintainCache(ctx context.Context) error {
	if !w.opts.CacheMaintenance {
		return nil
	}
	pending, err := w.c.CacheRemovalPending()
	if err != nil {
		return err
	}
	if !pending && time.Now().Before(w.nextCacheMaintenance) {
		return nil
	}
	if w.opts.Task != nil {
		end := w.opts.Task()
		defer end()
	}
	report, err := MaintainCache(ctx, w.db, w.c, w.opts.Namespace, w.cacheCursor, 4096, true)
	w.nextCacheMaintenance = time.Now().Add(10 * time.Second)
	if errors.Is(err, journal.ErrPending) && !pending {
		return nil
	}
	if err != nil {
		log.Printf("replica cache maintenance: %v", err)
		if pending {
			return err
		}
		return nil
	}
	w.cacheCursor = report.Next
	if report.Next == "" && report.Scanned != 0 {
		w.nextCacheMaintenance = time.Now().Add(15 * time.Minute)
	}
	if report.Removed > 0 {
		log.Printf("replica cache: removed %d obsolete snapshots, freed %d allocated bytes", report.Removed, report.AllocatedBytes)
	}
	return nil
}
