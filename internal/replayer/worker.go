package replayer

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Worker polls the API for jobs of one variant, replays them against the
// target, and uploads the results.
type Worker struct {
	Client   *Client
	Replayer *Replayer
	Log      *slog.Logger
	ID       string
	Variant  string
	Lease    time.Duration // default 5 minutes
	Poll     time.Duration // wait between polls when idle; default 2 seconds
	// IdleExit stops the worker after this long without finding a job. Zero
	// means run until cancelled. CI uses it so the step ends by itself.
	IdleExit time.Duration
}

// Stats counts what a worker did.
type Stats struct{ Jobs, Requests, Failures int }

// Run processes jobs until ctx is cancelled, IdleExit elapses, or the API
// rejects the worker's credentials. It returns what it processed.
func (w *Worker) Run(ctx context.Context) (Stats, error) {
	lease, poll := w.Lease, w.Poll
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	var st Stats
	idleSince := time.Now()
	backoff := poll

	for ctx.Err() == nil {
		job, err := w.Client.Claim(ctx, w.ID, w.Variant, lease)
		if err != nil {
			if errors.Is(err, ErrPermanent) {
				return st, err // wrong key or project: retrying only spams the API
			}
			w.Log.Warn("claim failed; will retry", "err", err, "retry_in", backoff)
			if !sleep(ctx, backoff) {
				break
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = poll

		if job == nil {
			if w.IdleExit > 0 && time.Since(idleSince) >= w.IdleExit {
				w.Log.Info("no more jobs; exiting", "idle_for", time.Since(idleSince).Round(time.Second))
				return st, nil
			}
			if !sleep(ctx, poll) {
				break
			}
			continue
		}
		idleSince = time.Now()

		started := time.Now()
		results := w.Replayer.Batch(ctx, job.Requests)
		if ctx.Err() != nil {
			// Shutting down mid-batch: release the job so another worker retries it now.
			_ = w.Client.Fail(context.WithoutCancel(ctx), w.ID, job.JobID, "worker shut down mid-batch")
			break
		}
		failed := 0
		for _, r := range results {
			if r.Error != "" {
				failed++
			}
		}

		err = w.Client.Complete(ctx, w.ID, job.JobID, results)
		switch {
		case err == nil:
			st.Jobs++
			st.Requests += len(results)
			st.Failures += failed
			w.Log.Info("batch done", "job_id", job.JobID, "variant", job.Variant, "requests", len(results),
				"errors", failed, "took_ms", time.Since(started).Milliseconds())
		case errors.Is(err, ErrLeaseLost):
			w.Log.Warn("lease lost; results discarded", "job_id", job.JobID)
		case errors.Is(err, ErrPermanent):
			// The API refused these results (for example, a bad payload).
			// Report it so the run does not wait for a batch that will never arrive.
			w.Log.Error("API rejected results", "job_id", job.JobID, "err", err)
			_ = w.Client.Fail(ctx, w.ID, job.JobID, err.Error())
		default:
			// Transient: the lease will expire and another attempt will redo the batch.
			w.Log.Warn("upload failed; the batch will be retried after its lease expires", "job_id", job.JobID, "err", err)
		}
	}
	return st, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
