package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/web"
)

type nasTreeView struct {
	ctx       context.Context
	db        *index.DB
	live      *syncRuntime
	namespace string
	mu        sync.Mutex
	verifying bool
	verified  time.Time
	loading   bool
	checked   time.Time
	problem   string
	wg        sync.WaitGroup
}

// Local verification must not wait behind a long network download or an
// inventory request. Only one bounded verification pass can run at a time.
func (v *nasTreeView) refreshProgress() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.live == nil || v.verifying || time.Since(v.verified) < 15*time.Second {
		return
	}
	v.verifying = true
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		ctx, cancel := context.WithTimeout(v.ctx, 5*time.Minute)
		defer cancel()
		_ = v.live.worker.VerifyLocalProgress(ctx)
		v.mu.Lock()
		v.verifying = false
		v.verified = time.Now()
		v.mu.Unlock()
	}()
}

func (v *nasTreeView) refresh() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.live == nil || v.loading || time.Since(v.checked) < 15*time.Second {
		return
	}
	v.loading = true
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		ctx, cancel := context.WithTimeout(v.ctx, 5*time.Minute)
		defer cancel()
		ready, stopped := make(chan struct{}, 1), make(chan struct{})
		go func() { defer close(stopped); v.live.network.Monitor.Watch(ctx, ready); cancel() }()
		defer func() { cancel(); <-stopped }()
		select {
		case <-ready:
		case <-ctx.Done():
		}
		var err error
		for ctx.Err() == nil {
			var cursor index.NASCursor
			cursor, err = v.db.NASCursor()
			if err != nil {
				break
			}
			page, e := v.live.inventory.ChangesPage(ctx, cursor.Namespace, cursor.Policy, cursor.Through, journal.MaxPage)
			err = e
			if err != nil {
				break
			}
			if page.Namespace != v.namespace {
				err = fmt.Errorf("NAS inventory identity changed")
				break
			}
			err = v.db.RecordNASPage(page)
			if err != nil || len(page.Batches) < journal.MaxPage {
				break
			}
		}
		if err == nil {
			err = ctx.Err()
		}
		v.mu.Lock()
		defer v.mu.Unlock()
		v.loading = false
		v.checked = time.Now()
		v.problem = ""
		if err != nil {
			log.Printf("NAS inventory refresh: %v", err)
			v.problem = "NAS inventory could not be refreshed. Showing the last received inventory."
		}
	}()
}

func (v *nasTreeView) controls() web.TreeControls {
	return web.TreeControls{
		Snapshot: func(ctx context.Context, path, after string) (any, error) {
			v.refreshProgress()
			v.refresh()
			tree, err := v.db.NASTree(ctx, path, after)
			if err != nil {
				return nil, err
			}
			v.mu.Lock()
			loading, problem := v.loading, v.problem
			v.mu.Unlock()
			active := ""
			if v.live != nil {
				active = v.live.worker.Status().ActivePath
			}
			return struct {
				index.NASTree
				Loading    bool   `json:"loading"`
				Problem    string `json:"problem"`
				ActivePath string `json:"activePath"`
			}{tree, loading, problem, active}, nil
		},
		Prioritize: func(ctx context.Context, path string) error {
			if v.live == nil {
				return fmt.Errorf("sync is disabled")
			}
			// A root request clears folder priority, returning to ordinary feed order.
			if path != "" {
				tree, err := v.db.NASTree(ctx, path, "")
				if err != nil {
					return err
				}
				if tree.Files == 0 && len(tree.Children) == 0 {
					return fmt.Errorf("directory has no known NAS files")
				}
			}
			if err := v.db.PrioritizeDirectory(path); err != nil {
				return err
			}
			v.live.worker.RequestDirectory()
			return nil
		},
	}
}
