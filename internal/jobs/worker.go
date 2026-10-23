// Package jobs runs queued background work (imports today; metadata fetching
// and link checks later) from the jobs table, so it survives restarts.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/marcelhoffs/burrow/internal/importexport"
	"github.com/marcelhoffs/burrow/internal/store"
)

// importBatch is how many bookmarks one transaction imports. Small enough
// that other users' writes never wait long on the single writer.
const importBatch = 500

type Worker struct {
	store *store.Store
	log   *slog.Logger
	wake  chan struct{}
}

func NewWorker(st *store.Store, log *slog.Logger) *Worker {
	return &Worker{store: st, log: log, wake: make(chan struct{}, 1)}
}

// Notify tells the worker a job was queued, so it needn't wait for its poll.
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if err := w.store.RequeueRunning(ctx); err != nil {
		w.log.Error("requeue jobs", "err", err)
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		for w.RunOnce(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-t.C:
		}
	}
}

// RunOnce claims and runs one job; it reports whether there was one.
func (w *Worker) RunOnce(ctx context.Context) bool {
	j, payload, err := w.store.ClaimJob(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("claim job", "err", err)
		}
		return false
	}
	var result any
	switch j.Kind {
	case store.JobImport:
		result, err = w.runImport(ctx, j, payload)
	default:
		err = fmt.Errorf("unknown job kind %q", j.Kind)
	}
	b, _ := json.Marshal(result)
	if ferr := w.store.FinishJob(ctx, j.ID, string(b), err); ferr != nil {
		w.log.Error("finish job", "job", j.ID, "err", ferr)
	}
	if err != nil {
		w.log.Warn("job failed", "job", j.ID, "kind", j.Kind, "err", err)
	} else {
		w.log.Info("job done", "job", j.ID, "kind", j.Kind, "result", string(b))
	}
	return true
}

func (w *Worker) runImport(ctx context.Context, j store.Job, payload string) (store.ImportResult, error) {
	var total store.ImportResult
	items, err := importexport.Parse([]byte(payload))
	if err != nil {
		return total, err
	}
	w.store.JobProgress(ctx, j.ID, 0, len(items))
	for start := 0; start < len(items); start += importBatch {
		end := min(start+importBatch, len(items))
		r, err := w.store.ImportBookmarks(ctx, j.UserID, items[start:end])
		if err != nil {
			return total, err
		}
		total.Created += r.Created
		total.Merged += r.Merged
		total.Skipped += r.Skipped
		w.store.JobProgress(ctx, j.ID, end, len(items))
	}
	return total, nil
}
