// Package replayer re-sends recorded requests to a target service and reports
// what came back. It runs on the CI runner next to the baseline or candidate
// build; credentials for the target come from the runner's environment and are
// never sent to, or stored by, the replaygate API.
package replayer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Sangsty/replaygate/internal/sanitize"
)

// Request is a recorded request to replay.
type Request struct {
	ID      int64               `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// Result is what the target answered for one request.
type Result struct {
	RequestID int64  `json:"request_id"`
	Status    int    `json:"status,omitempty"`
	Body      []byte `json:"body,omitempty"`
	Truncated bool   `json:"truncated"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Replayer sends requests to Target.
type Replayer struct {
	Target      *url.URL
	AuthHeader  string // full Authorization header value; empty sends none
	Sanitizer   *sanitize.Sanitizer
	MaxBody     int // bytes kept per response; larger bodies are cut and flagged
	Concurrency int
	Timeout     time.Duration
	client      *http.Client
	once        sync.Once
}

const (
	defaultMaxBody     = 256 << 10
	defaultConcurrency = 8
	defaultTimeout     = 10 * time.Second
)

func (r *Replayer) init() {
	r.once.Do(func() {
		if r.MaxBody <= 0 {
			r.MaxBody = defaultMaxBody
		}
		if r.Concurrency <= 0 {
			r.Concurrency = defaultConcurrency
		}
		if r.Timeout <= 0 {
			r.Timeout = defaultTimeout
		}
		r.client = &http.Client{
			Timeout: r.Timeout,
			// A redirect is part of the response under test; following it would
			// compare the destination instead of the service's own behaviour.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
}

// skipHeader lists headers that must not be copied from a recording: transport
// headers the HTTP client sets itself, and Accept-Encoding, which would make
// Go skip transparent decompression and hand us compressed bytes to diff.
var skipHeader = map[string]bool{
	"host": true, "content-length": true, "accept-encoding": true, "connection": true,
	"keep-alive": true, "proxy-connection": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
}

// Do replays one request. Network failures are reported in Result.Error, not
// returned: a candidate that cannot answer is a finding, not a worker fault.
func (r *Replayer) Do(ctx context.Context, req Request) Result {
	r.init()
	res := Result{RequestID: req.ID}

	u := *r.Target
	u.Path = strings.TrimRight(r.Target.Path, "/") + req.Path
	u.RawPath = ""
	u.RawQuery = req.Query

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		res.Error = "build request: " + err.Error()
		return res
	}
	for name, values := range req.Headers {
		if skipHeader[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			if v == sanitize.Redacted { // a placeholder, not a real value
				continue
			}
			hr.Header.Add(name, v)
		}
	}
	if r.AuthHeader != "" {
		hr.Header.Set("Authorization", r.AuthHeader)
	}
	hr.Header.Set("User-Agent", "replaygate-replayer/1")

	start := time.Now()
	resp, err := r.client.Do(hr)
	if err != nil {
		res.LatencyMS = int(time.Since(start).Milliseconds())
		res.Error = errorText(err)
		return res
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(r.MaxBody)+1))
	res.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Error = "read response: " + errorText(err)
		return res
	}
	res.Status = resp.StatusCode
	if len(raw) > r.MaxBody {
		raw, res.Truncated = raw[:r.MaxBody], true
	}
	if r.Sanitizer != nil {
		raw = r.Sanitizer.Body(raw)
	}
	res.Body = raw
	return res
}

// errorText shortens a client error to something safe and readable. It drops
// the request URL that net/http embeds, which may include a query string.
func errorText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return err.Error()
}

// Batch replays requests with bounded concurrency and returns results in the
// same order as the input.
func (r *Replayer) Batch(ctx context.Context, reqs []Request) []Result {
	r.init()
	out := make([]Result, len(reqs))
	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = r.Do(ctx, reqs[i])
		}(i)
	}
	wg.Wait()
	return out
}
