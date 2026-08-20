package webhook

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultBatchSize is how many events one poll claims at a time.
const DefaultBatchSize = 20

// DefaultPollInterval is how often the worker checks for due events.
const DefaultPollInterval = 5 * time.Second


type Worker struct {
	repo         *Repository
	svc          *Service
	batchSize    int
	pollInterval time.Duration
	logger       *slog.Logger
}

// WorkerOption configures optional Worker behavior.
type WorkerOption func(*Worker)

func WithBatchSize(n int) WorkerOption              { return func(w *Worker) { w.batchSize = n } }
func WithPollInterval(d time.Duration) WorkerOption { return func(w *Worker) { w.pollInterval = d } }
func WithWorkerLogger(l *slog.Logger) WorkerOption  { return func(w *Worker) { w.logger = l } }

func NewWorker(repo *Repository, svc *Service, opts ...WorkerOption) *Worker {
	w := &Worker{
		repo:         repo,
		svc:          svc,
		batchSize:    DefaultBatchSize,
		pollInterval: DefaultPollInterval,
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}


func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("webhook worker: shutting down")
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

// processBatch claims one batch and delivers every event in it
// concurrently. Claiming happens in its own short transaction that
// commits immediately after marking rows 'processing' -- the row lock
// FOR UPDATE SKIP LOCKED takes during ClaimPendingEvents is released the
// moment that transaction commits, well before any slow outbound HTTP
// call starts. Holding a Postgres row lock open for the duration of a
// network call to an arbitrary third party would be a real problem --
// this design deliberately avoids that.
func (w *Worker) processBatch(ctx context.Context) {
	tx, err := w.repo.BeginTx(ctx, nil)
	if err != nil {
		w.logger.Error("webhook worker: begin claim tx", "error", err)
		return
	}

	events, err := w.repo.ClaimPendingEvents(ctx, tx, w.batchSize)
	if err != nil {
		w.logger.Error("webhook worker: claim pending events", "error", err)
		_ = tx.Rollback()
		return
	}
	if err := tx.Commit(); err != nil {
		w.logger.Error("webhook worker: commit claim tx", "error", err)
		return
	}
	if len(events) == 0 {
		return
	}

	w.logger.Info("webhook worker: claimed batch", "count", len(events))

	var wg sync.WaitGroup
	for _, ev := range events {
		wg.Add(1)
		go func(ev *Event) {
			defer wg.Done()
			w.svc.ProcessEvent(ctx, ev)
		}(ev)
	}
	wg.Wait()
}