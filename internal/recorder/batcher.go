package recorder

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sangsty/replaygate/internal/model"
)

// Uploader sends a batch to the API. Implementations must not retain the slice
// after returning.
type Uploader interface {
	Upload(ctx context.Context, reqs []model.RecordedRequest) error
}

// UploaderFunc adapts a function to Uploader.
type UploaderFunc func(ctx context.Context, reqs []model.RecordedRequest) error

// Upload implements Uploader.
func (f UploaderFunc) Upload(ctx context.Context, reqs []model.RecordedRequest) error {
	return f(ctx, reqs)
}

// ErrPermanent marks an upload error that retrying cannot fix (for example a
// rejected API key), so the batcher gives up on the batch immediately.
var ErrPermanent = errors.New("permanent upload error")

// BatcherConfig tunes a Batcher. Zero values get sensible defaults.
type BatcherConfig struct {
	QueueSize     int           // buffered requests before new ones are dropped (default 10000)
	BatchSize     int           // requests per upload (default 100)
	FlushInterval time.Duration // flush a partial batch after this long (default 1s)
	MaxRetries    int           // retries after the first attempt (0 = default 3, negative = none)
	RetryBase     time.Duration // first retry delay, doubled each time (default 200ms)
	UploadTimeout time.Duration // per-attempt timeout (default 10s)
}

func (c *BatcherConfig) applyDefaults() {
	if c.QueueSize <= 0 {
		c.QueueSize = 10000
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = time.Second
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	} else if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 200 * time.Millisecond
	}
	if c.UploadTimeout <= 0 {
		c.UploadTimeout = 10 * time.Second
	}
}

// Batcher collects requests and uploads them in batches. Submit never blocks:
// when the queue is full the request is dropped and counted, because slowing
// down live traffic to record it would defeat the point of a recorder.
type Batcher struct {
	cfg BatcherConfig
	up  Uploader
	log *slog.Logger

	mu     sync.RWMutex // guards closed and sends on ch
	closed bool
	ch     chan model.RecordedRequest
	done   chan struct{}

	dropped  atomic.Int64
	uploaded atomic.Int64
}

// NewBatcher starts a Batcher. Call Close to flush and stop it.
func NewBatcher(up Uploader, cfg BatcherConfig, log *slog.Logger) *Batcher {
	cfg.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	b := &Batcher{
		cfg:  cfg,
		up:   up,
		log:  log,
		ch:   make(chan model.RecordedRequest, cfg.QueueSize),
		done: make(chan struct{}),
	}
	go b.run()
	return b
}

// Submit queues a request without blocking. It implements Sink.
func (b *Batcher) Submit(r model.RecordedRequest) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		b.dropped.Add(1)
		return
	}
	select {
	case b.ch <- r:
	default:
		b.dropped.Add(1)
	}
}

// Dropped is the number of requests discarded because the queue was full, the
// batcher was closed, or an upload failed for good.
func (b *Batcher) Dropped() int64 { return b.dropped.Load() }

// Uploaded is the number of requests the API accepted.
func (b *Batcher) Uploaded() int64 { return b.uploaded.Load() }

// Close stops accepting requests, uploads what is queued, and waits until that
// finishes or ctx expires.
func (b *Batcher) Close(ctx context.Context) error {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		close(b.ch)
	}
	b.mu.Unlock()

	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Batcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]model.RecordedRequest, 0, b.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.send(batch)
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-b.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= b.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// send uploads one batch, retrying transient failures with exponential backoff.
func (b *Batcher) send(batch []model.RecordedRequest) {
	delay := b.cfg.RetryBase
	var err error
	for attempt := 0; attempt <= b.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), b.cfg.UploadTimeout)
		err = b.up.Upload(ctx, batch)
		cancel()
		if err == nil {
			b.uploaded.Add(int64(len(batch)))
			return
		}
		if errors.Is(err, ErrPermanent) {
			break
		}
	}
	b.dropped.Add(int64(len(batch)))
	b.log.Error("dropping batch after failed upload", "requests", len(batch), "err", err)
}
