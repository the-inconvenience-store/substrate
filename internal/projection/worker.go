package projection

import (
	"context"

	"github.com/google/uuid"
)

type backfillJob struct{ ws, col uuid.UUID }

// Worker runs backfills asynchronously off a buffered queue. It satisfies
// schema.BackfillEnqueuer.
type Worker struct {
	bf *Backfiller
	ch chan backfillJob
}

func NewWorker(bf *Backfiller, buffer int) *Worker {
	if buffer <= 0 {
		buffer = 256
	}
	return &Worker{bf: bf, ch: make(chan backfillJob, buffer)}
}

// Enqueue is non-blocking. A full buffer drops the signal; the next activation or a
// manual run still converges (backfill is idempotent).
func (w *Worker) Enqueue(ws, col uuid.UUID) {
	select {
	case w.ch <- backfillJob{ws: ws, col: col}:
	default:
	}
}

// Run drains the queue until ctx is cancelled, backfilling each collection to completion.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.ch:
			_, _ = w.bf.Run(ctx, job.ws, job.col, defaultBatch)
		}
	}
}
