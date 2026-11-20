// Package analyzer turns the raw replay responses of one run into a verdict.
//
// For every recorded request there are up to three observations: baseline_a,
// baseline_b (the same baseline build replayed twice) and the candidate.
// Fields that differ between A and B change without any code change (ids,
// timestamps, tokens), so they are learned as noise per endpoint and excluded
// when the candidate is compared against A.
package analyzer

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Sangsty/replaygate/internal/diff"
)

// Config is the per-project (and per-run) comparison configuration. It mirrors
// the stored projects.diff_config JSON.
type Config struct {
	FloatTolerance float64              `json:"float_tolerance"`
	ArrayModes     map[string]ArrayMode `json:"array_modes"`
	Latency        struct {
		Ratio      float64 `json:"ratio"`
		MinDeltaMS int     `json:"min_delta_ms"`
	} `json:"latency"`
	FailOn              []string `json:"fail_on"`
	ValueChangeSeverity string   `json:"value_change_severity"`
	IgnorePaths         []string `json:"ignore_paths"`
	// NoiseLearning defaults to true. Turning it off is how the evaluation
	// measures the false-positive rate without noise learning.
	NoiseLearning *bool `json:"noise_learning"`
}

// ArrayMode says how one array path is compared: as a set keyed by a field, or
// as an unordered list of scalars. Absent means in-order.
type ArrayMode struct {
	Key       string `json:"key,omitempty"`
	Unordered bool   `json:"unordered,omitempty"`
}

// ParseConfig decodes stored JSON, applying defaults for missing keys.
// Empty input yields the defaults.
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return Config{}, fmt.Errorf("invalid diff config: %w", err)
		}
	}
	if len(c.FailOn) == 0 {
		c.FailOn = []string{string(diff.Breaking)}
	}
	for _, s := range c.FailOn {
		switch diff.Severity(s) {
		case diff.Breaking, diff.Additive, diff.Latency:
		default:
			return Config{}, fmt.Errorf("invalid diff config: unknown fail_on severity %q", s)
		}
	}
	switch diff.Severity(c.ValueChangeSeverity) {
	case "", diff.Breaking, diff.Additive:
	default:
		return Config{}, fmt.Errorf("invalid diff config: value_change_severity must be breaking or additive, got %q", c.ValueChangeSeverity)
	}
	if c.FloatTolerance < 0 {
		return Config{}, fmt.Errorf("invalid diff config: float_tolerance must not be negative")
	}
	return c, nil
}

// failOnSet returns the severities that fail a run. An empty list means the
// default (breaking), so a zero-value Config is safe to use.
func (c Config) failOnSet() map[diff.Severity]bool {
	set := map[diff.Severity]bool{}
	if len(c.FailOn) == 0 {
		set[diff.Breaking] = true
		return set
	}
	for _, s := range c.FailOn {
		set[diff.Severity(s)] = true
	}
	return set
}

// LearnsNoise reports whether noise learning is enabled.
func (c Config) LearnsNoise() bool { return c.NoiseLearning == nil || *c.NoiseLearning }

// diffConfig converts to the diff engine's configuration, adding extra ignores.
func (c Config) diffConfig(extraIgnore []string) diff.Config {
	dc := diff.Config{
		FloatTolerance:      c.FloatTolerance,
		ValueChangeSeverity: diff.Severity(c.ValueChangeSeverity),
		LatencyFactor:       c.Latency.Ratio,
		LatencyMinMS:        c.Latency.MinDeltaMS,
		ArrayKeys:           map[string]string{},
	}
	paths := make([]string, 0, len(c.ArrayModes))
	for p := range c.ArrayModes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m := c.ArrayModes[p]
		switch {
		case m.Key != "":
			dc.ArrayKeys[p] = m.Key
		case m.Unordered:
			dc.UnorderedArrays = append(dc.UnorderedArrays, p)
		}
	}
	dc.IgnorePaths = append(append([]string(nil), c.IgnorePaths...), extraIgnore...)
	return dc
}

// Observation is everything replayed for one recorded request.
type Observation struct {
	RequestID int64
	Endpoint  string
	A         *diff.Response // baseline_a
	B         *diff.Response // baseline_b; nil when noise learning is off
	Candidate *diff.Response
}

// RequestFinding is one finding attributed to a request.
type RequestFinding struct {
	RequestID int64
	Endpoint  string
	diff.Finding
}

// Result is the verdict of a run.
type Result struct {
	Findings []RequestFinding
	// Noise maps endpoint to the normalized JSON paths excluded from the diff.
	Noise map[string][]string
	// Counts of findings by severity.
	Breaking, Additive, Latency int
	// Compared is the number of requests with both a baseline and a candidate.
	Compared int
	// Observed is the number of requests with a baseline (A) response, and
	// BaselineErrors how many of those failed at the network level. A baseline
	// that is down produces no findings, so callers must not trust a pass when
	// most baseline replays failed (see BaselineUnhealthy).
	Observed, BaselineErrors int
	// Passed is false when any finding has a severity listed in fail_on.
	Passed bool
}

// LearnNoise returns, per endpoint, the paths whose value differs between the
// two baseline replays. Requests where either replay failed teach nothing.
func LearnNoise(obs []Observation, cfg Config) map[string][]string {
	set := map[string]map[string]struct{}{}
	base := cfg.diffConfig(nil)
	base.LatencyFactor = 0 // latency jitter between A and B is not field noise
	for _, o := range obs {
		if o.A == nil || o.B == nil || o.A.Err != "" || o.B.Err != "" {
			continue
		}
		for _, f := range diff.Compare(*o.A, *o.B, base) {
			if f.Kind == diff.StatusChanged || f.Kind == diff.LatencyRegression || f.Kind == diff.RequestFailed {
				continue
			}
			if set[o.Endpoint] == nil {
				set[o.Endpoint] = map[string]struct{}{}
			}
			set[o.Endpoint][diff.NormalizeIndexed(f.Path)] = struct{}{}
		}
	}
	out := make(map[string][]string, len(set))
	for ep, paths := range set {
		for p := range paths {
			out[ep] = append(out[ep], p)
		}
		sort.Strings(out[ep])
	}
	return out
}

// Analyze compares each request's candidate against baseline A and decides
// whether the run passes.
func Analyze(obs []Observation, cfg Config) Result {
	res := Result{Passed: true}
	if cfg.LearnsNoise() {
		res.Noise = LearnNoise(obs, cfg)
	}
	failOn := cfg.failOnSet()

	for _, o := range obs {
		if o.A != nil {
			res.Observed++
			if o.A.Err != "" {
				res.BaselineErrors++
			}
		}
		if o.A == nil || o.Candidate == nil {
			continue // incomplete observation; the run-level check handles missing batches
		}
		res.Compared++
		dc := cfg.diffConfig(res.Noise[o.Endpoint])
		for _, f := range diff.Compare(*o.A, *o.Candidate, dc) {
			res.Findings = append(res.Findings, RequestFinding{RequestID: o.RequestID, Endpoint: o.Endpoint, Finding: f})
			switch f.Severity {
			case diff.Breaking:
				res.Breaking++
			case diff.Additive:
				res.Additive++
			case diff.Latency:
				res.Latency++
			}
			if failOn[f.Severity] {
				res.Passed = false
			}
		}
	}
	return res
}

// BaselineUnhealthy reports whether the run cannot be trusted because the
// baseline did not answer: nothing could be compared, or more than half of the
// baseline replays failed. A green check in that state would be a lie.
func (r Result) BaselineUnhealthy() (bool, string) {
	switch {
	case r.Compared == 0:
		return true, "no request had both a baseline and a candidate response"
	case r.BaselineErrors*2 > r.Observed:
		return true, fmt.Sprintf("the baseline failed for %d of %d requests; check that the baseline build is running and reachable", r.BaselineErrors, r.Observed)
	}
	return false, ""
}
