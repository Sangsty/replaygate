// Package recorder is the reverse proxy that records sanitized traffic.
package recorder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/sanitize"
)

// Sink receives sanitized requests. Submit must never block the caller.
type Sink interface {
	Submit(model.RecordedRequest)
}

// Options configures a Proxy.
type Options struct {
	// Upstream is the service being recorded. Required.
	Upstream *url.URL
	// Sink receives recorded requests. Required.
	Sink Sink
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// MaxBodyBytes caps the request body that is recorded (default model.MaxBodyBytes).
	MaxBodyBytes int64
	// Transport defaults to a clone of http.DefaultTransport.
	Transport http.RoundTripper
}

// Proxy forwards every request to the upstream and records a sanitized copy of
// each GET. Recording never changes what the client or the upstream sees, and
// it never blocks a response: sanitizing happens after the response has been
// written and the hand-off to the Sink is non-blocking.
//
// Until sanitizer rules are loaded (see RefreshRules) the proxy forwards
// traffic but records nothing, so a rules outage fails closed instead of
// storing unsanitized data.
type Proxy struct {
	rp      *httputil.ReverseProxy
	sink    Sink
	san     atomic.Pointer[sanitize.Sanitizer]
	maxBody int64
	log     *slog.Logger
}

type stateKey struct{}

// reqState is shared between ServeHTTP and the reverse proxy's error handler.
type reqState struct{ upstreamFailed bool }

// NewProxy builds a Proxy.
func NewProxy(o Options) *Proxy {
	p := &Proxy{
		sink:    o.Sink,
		maxBody: o.MaxBodyBytes,
		log:     o.Logger,
	}
	if p.maxBody <= 0 {
		p.maxBody = model.MaxBodyBytes
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	transport := o.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConnsPerHost = 100
		transport = t
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(o.Upstream)
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if st, ok := r.Context().Value(stateKey{}).(*reqState); ok {
				st.upstreamFailed = true
			}
			p.log.Warn("upstream error", "method", r.Method, "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

// SetRules compiles and installs sanitizer rules. On error the previous rules
// stay in effect.
func (p *Proxy) SetRules(r sanitize.Rules) error {
	s, err := sanitize.New(r)
	if err != nil {
		return err
	}
	p.san.Store(s)
	return nil
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	san := p.san.Load()
	if r.Method != http.MethodGet || san == nil || isUpgrade(r) {
		p.rp.ServeHTTP(w, r)
		return
	}

	body, recordable := p.captureBody(r)
	hdr := r.Header.Clone() // snapshot: the proxy rewrites headers on its own copy, but be explicit
	st := &reqState{}
	r = r.WithContext(context.WithValue(r.Context(), stateKey{}, st))
	sw := &statusWriter{ResponseWriter: w}

	start := time.Now()
	p.rp.ServeHTTP(sw, r)
	elapsed := time.Since(start)

	if !recordable || st.upstreamFailed {
		return // an infrastructure failure is not API behavior worth replaying
	}
	p.sink.Submit(model.RecordedRequest{
		Method:    r.Method,
		Path:      escapedPath(r),
		Query:     san.Query(r.URL.RawQuery),
		Headers:   san.Headers(recordableHeaders(hdr)),
		Body:      san.Body(body),
		Status:    sw.statusCode(),
		LatencyMS: int(elapsed.Milliseconds()),
	})
}

// captureBody reads up to maxBody bytes of the request body for recording and
// puts everything it consumed back in front of the rest, so the upstream still
// receives the complete body. recordable is false when the body was too large
// or unreadable: a truncated body would not be a faithful replay.
func (p *Proxy) captureBody(r *http.Request) (body []byte, recordable bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, p.maxBody+1))
	rest := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), rest), rest}
	if err != nil || int64(len(buf)) > p.maxBody {
		return nil, false
	}
	return buf, true
}

func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != ""
}

func escapedPath(r *http.Request) string {
	if p := r.URL.EscapedPath(); p != "" {
		return p
	}
	return "/"
}

// skipHeaders are transport details, not application behavior: hop-by-hop
// headers per RFC 9110, plus content-length and accept-encoding (the replayer
// negotiates its own encoding so it can read the response body).
var skipHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {}, // also in sanitize's hard floor; dropped here as well
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"content-length":      {},
	"accept-encoding":     {},
}

func recordableHeaders(h http.Header) map[string][]string {
	extra := map[string]struct{}{} // headers named by the Connection header are hop-by-hop too
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			extra[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
		}
	}
	out := make(map[string][]string, len(h))
	for name, vals := range h {
		lower := strings.ToLower(name)
		if _, skip := skipHeaders[lower]; skip {
			continue
		}
		if _, skip := extra[lower]; skip {
			continue
		}
		out[name] = vals
	}
	return out
}

// statusWriter remembers the final status code written to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	// Informational 1xx responses (except 101) are followed by the real one.
	if !w.wrote && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (flush, deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) statusCode() int {
	if !w.wrote {
		return http.StatusOK // handler wrote nothing: net/http sends 200
	}
	return w.status
}

// RefreshRules loads sanitizer rules and keeps them fresh until ctx is done.
// Until the first successful load it retries every retry interval; afterwards
// it reloads every `every`. A failed reload keeps the last good rules.
func (p *Proxy) RefreshRules(ctx context.Context, fetch func(context.Context) (sanitize.Rules, error), every, retry time.Duration) {
	loaded := false
	wait := time.Duration(0)
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		rules, err := fetch(ctx)
		if err == nil {
			err = p.SetRules(rules)
		}
		switch {
		case err != nil && errors.Is(ctx.Err(), context.Canceled):
			return
		case err != nil:
			p.log.Warn("loading sanitizer rules failed", "err", err, "recording", loaded)
			wait = every
			if !loaded {
				wait = retry
			}
		default:
			if !loaded {
				p.log.Info("sanitizer rules loaded; recording enabled", "rules", rules.String())
			}
			loaded = true
			wait = every
		}
	}
}
