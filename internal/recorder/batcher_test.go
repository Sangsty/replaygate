package recorder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/model"
)

func rr(path string) model.RecordedRequest {
	return model.RecordedRequest{Method: "GET", Path: path, Status: 200}
}

// recUploader records batches and can fail on demand.
type recUploader struct {
	mu      sync.Mutex
	batches [][]model.RecordedRequest
	calls   atomic.Int32
	errs    []error // returned in order, one per call; nil entries mean success
	gate    chan struct{}
	got     chan struct{} // signalled after each successful upload
}

func newRecUploader(errs ...error) *recUploader {
	return &recUploader{errs: errs, got: make(chan struct{}, 100)}
}

func (u *recUploader) Upload(ctx context.Context, reqs []model.RecordedRequest) error {
	if u.gate != nil {
		select {
		case <-u.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	n := int(u.calls.Add(1))
	if n <= len(u.errs) && u.errs[n-1] != nil {
		return u.errs[n-1]
	}
	u.mu.Lock()
	u.batches = append(u.batches, append([]model.RecordedRequest(nil), reqs...))
	u.mu.Unlock()
	select { // signal waiters, but never block the batcher if nobody is listening
	case u.got <- struct{}{}:
	default:
	}
	return nil
}

func (u *recUploader) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, b := range u.batches {
		n += len(b)
	}
	return n
}

func waitUpload(t *testing.T, u *recUploader) {
	t.Helper()
	select {
	case <-u.got:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an upload")
	}
}

// eventually polls cond until it is true or two seconds pass. The batcher
// bumps its counters after Upload returns, so a test that was woken by the fake
// uploader must wait for them instead of reading them straight away.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func fastCfg() BatcherConfig {
	return BatcherConfig{BatchSize: 100, FlushInterval: time.Hour, MaxRetries: 3, RetryBase: time.Millisecond, UploadTimeout: time.Second}
}

func closeBatcher(t *testing.T, b *Batcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBatcherFlushesWhenBatchIsFull(t *testing.T) {
	u := newRecUploader()
	cfg := fastCfg()
	cfg.BatchSize = 3
	b := NewBatcher(u, cfg, discard)
	defer closeBatcher(t, b)

	for i := 0; i < 3; i++ {
		b.Submit(rr(fmt.Sprintf("/%d", i)))
	}
	waitUpload(t, u)
	if len(u.batches) != 1 || len(u.batches[0]) != 3 {
		t.Fatalf("batches = %v, want one batch of 3", u.batches)
	}
	eventually(t, "Uploaded to reach 3", func() bool { return b.Uploaded() == 3 })
}

func TestBatcherFlushesOnInterval(t *testing.T) {
	u := newRecUploader()
	cfg := fastCfg()
	cfg.FlushInterval = 10 * time.Millisecond
	b := NewBatcher(u, cfg, discard)
	defer closeBatcher(t, b)

	b.Submit(rr("/lonely"))
	waitUpload(t, u)
	if u.total() != 1 {
		t.Errorf("uploaded %d requests, want 1", u.total())
	}
}

func TestBatcherDropsWhenQueueIsFull(t *testing.T) {
	u := newRecUploader()
	u.gate = make(chan struct{}) // uploads block until released
	cfg := fastCfg()
	cfg.QueueSize = 2
	cfg.BatchSize = 1
	b := NewBatcher(u, cfg, discard)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < 10; i++ {
			b.Submit(rr(fmt.Sprintf("/%d", i)))
		}
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked while the queue was full")
	}
	// At most the in-flight request plus a full queue can be kept.
	if b.Dropped() < 5 {
		t.Errorf("Dropped = %d, want at least 5 of 10", b.Dropped())
	}

	close(u.gate)
	closeBatcher(t, b)
	if got := b.Uploaded() + b.Dropped(); got != 10 {
		t.Errorf("uploaded+dropped = %d, want 10 (nothing lost silently)", got)
	}
}

func TestBatcherRetriesTransientFailures(t *testing.T) {
	u := newRecUploader(errors.New("503"), errors.New("503"))
	cfg := fastCfg()
	cfg.BatchSize = 2
	b := NewBatcher(u, cfg, discard)
	defer closeBatcher(t, b)

	b.Submit(rr("/a"))
	b.Submit(rr("/b"))
	waitUpload(t, u)

	if got := u.calls.Load(); got != 3 {
		t.Errorf("upload calls = %d, want 3 (two failures, then success)", got)
	}
	eventually(t, "Uploaded to reach 2", func() bool { return b.Uploaded() == 2 })
	if b.Dropped() != 0 {
		t.Errorf("dropped = %d, want 0", b.Dropped())
	}
}

func TestBatcherGivesUpAfterMaxRetries(t *testing.T) {
	boom := errors.New("down")
	u := newRecUploader(boom, boom, boom, boom, boom, boom)
	cfg := fastCfg()
	cfg.BatchSize = 2
	cfg.MaxRetries = 2
	b := NewBatcher(u, cfg, discard)

	b.Submit(rr("/a"))
	b.Submit(rr("/b"))
	closeBatcher(t, b)

	if got := u.calls.Load(); got != 3 {
		t.Errorf("upload calls = %d, want 3 (1 attempt + 2 retries)", got)
	}
	if b.Dropped() != 2 || b.Uploaded() != 0 {
		t.Errorf("dropped=%d uploaded=%d, want 2/0", b.Dropped(), b.Uploaded())
	}
}

func TestBatcherDoesNotRetryPermanentErrors(t *testing.T) {
	u := newRecUploader(fmt.Errorf("%w: 401", ErrPermanent), errors.New("would succeed on retry"))
	cfg := fastCfg()
	cfg.BatchSize = 1
	b := NewBatcher(u, cfg, discard)

	b.Submit(rr("/a"))
	closeBatcher(t, b)

	if got := u.calls.Load(); got != 1 {
		t.Errorf("upload calls = %d, want 1 (permanent errors are not retried)", got)
	}
	if b.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", b.Dropped())
	}
}

func TestBatcherCloseDrainsQueue(t *testing.T) {
	u := newRecUploader()
	b := NewBatcher(u, fastCfg(), discard) // batch size 100 and a 1h interval: only Close can flush
	for i := 0; i < 5; i++ {
		b.Submit(rr(fmt.Sprintf("/%d", i)))
	}
	closeBatcher(t, b)
	if u.total() != 5 {
		t.Errorf("uploaded %d requests after Close, want 5", u.total())
	}
}

func TestBatcherSubmitAfterCloseIsSafe(t *testing.T) {
	u := newRecUploader()
	b := NewBatcher(u, fastCfg(), discard)
	closeBatcher(t, b)
	closeBatcher(t, b) // closing twice must not panic

	b.Submit(rr("/late")) // must not panic on a closed channel
	if b.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", b.Dropped())
	}
}

func TestBatcherConcurrentSubmit(t *testing.T) {
	u := newRecUploader()
	cfg := fastCfg()
	cfg.BatchSize = 7
	cfg.QueueSize = 10000
	b := NewBatcher(u, cfg, discard)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				b.Submit(rr("/x"))
			}
		}()
	}
	wg.Wait()
	closeBatcher(t, b)

	if got := u.total(); got != 800 {
		t.Errorf("uploaded %d requests, want 800", got)
	}
}
