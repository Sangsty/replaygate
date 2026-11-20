package analyzer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Sangsty/replaygate/internal/diff"
)

func r(body string) *diff.Response {
	return &diff.Response{Status: 200, Body: []byte(body), LatencyMS: 10}
}

func boolp(b bool) *bool { return &b }

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{"empty gets defaults", ``, "", func(t *testing.T, c Config) {
			if !reflect.DeepEqual(c.FailOn, []string{"breaking"}) || !c.LearnsNoise() {
				t.Errorf("defaults wrong: %+v", c)
			}
		}},
		{"stored project default", `{"float_tolerance":1e-9,"array_modes":{},"latency":{"ratio":1.5,"min_delta_ms":50},"fail_on":["breaking"]}`, "",
			func(t *testing.T, c Config) {
				if c.FloatTolerance != 1e-9 || c.Latency.Ratio != 1.5 || c.Latency.MinDeltaMS != 50 {
					t.Errorf("parsed wrong: %+v", c)
				}
			}},
		{"noise learning off", `{"noise_learning":false}`, "", func(t *testing.T, c Config) {
			if c.LearnsNoise() {
				t.Error("noise learning should be off")
			}
		}},
		{"array modes", `{"array_modes":{"$.items":{"key":"id"},"$.tags":{"unordered":true}}}`, "", func(t *testing.T, c Config) {
			dc := c.diffConfig(nil)
			if dc.ArrayKeys["$.items"] != "id" || !reflect.DeepEqual(dc.UnorderedArrays, []string{"$.tags"}) {
				t.Errorf("array modes not translated: %+v", dc)
			}
		}},
		{"malformed JSON", `{`, "invalid diff config", nil},
		{"unknown severity", `{"fail_on":["catastrophic"]}`, "unknown fail_on", nil},
		{"bad value severity", `{"value_change_severity":"latency"}`, "value_change_severity", nil},
		{"negative tolerance", `{"float_tolerance":-1}`, "negative", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestLearnNoise(t *testing.T) {
	obs := []Observation{
		{RequestID: 1, Endpoint: "/users/{id}", A: r(`{"id":1,"ts":100,"name":"a"}`), B: r(`{"id":1,"ts":101,"name":"a"}`)},
		{RequestID: 2, Endpoint: "/users/{id}", A: r(`{"id":2,"ts":200,"name":"b"}`), B: r(`{"id":2,"ts":201,"name":"b"}`)},
		{RequestID: 3, Endpoint: "/items", A: r(`{"rows":[{"t":1},{"t":2}]}`), B: r(`{"rows":[{"t":9},{"t":8}]}`)},
		{RequestID: 4, Endpoint: "/stable", A: r(`{"a":1}`), B: r(`{"a":1}`)},
		{RequestID: 5, Endpoint: "/failed", A: r(`{"a":1}`), B: &diff.Response{Err: "timeout"}},
		{RequestID: 6, Endpoint: "/nob", A: r(`{"a":1}`)},
	}
	got := LearnNoise(obs, Config{})
	want := map[string][]string{
		"/users/{id}": {"$.ts"},
		"/items":      {"$.rows[*].t"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("noise = %v, want %v", got, want)
	}
}

func TestLearnNoiseIgnoresStatusAndLatencyJitter(t *testing.T) {
	cfg := Config{}
	cfg.Latency.Ratio = 1.1
	a := &diff.Response{Status: 200, Body: []byte(`{}`), LatencyMS: 10}
	b := &diff.Response{Status: 500, Body: []byte(`{}`), LatencyMS: 900}
	got := LearnNoise([]Observation{{Endpoint: "/x", A: a, B: b}}, cfg)
	if len(got) != 0 {
		t.Errorf("status/latency differences must not become field noise, got %v", got)
	}
}

// The scenario that motivates noise learning: a field that changes on every
// call fails every PR unless it is learned as noise.
func TestNoiseLearningRemovesFalsePositives(t *testing.T) {
	obs := []Observation{
		{RequestID: 1, Endpoint: "/orders/{id}",
			A:         r(`{"id":1,"updated_at":"t1","total":10}`),
			B:         r(`{"id":1,"updated_at":"t2","total":10}`),
			Candidate: r(`{"id":1,"updated_at":"t3","total":10}`)},
	}

	on := Analyze(obs, Config{})
	if !on.Passed || on.Breaking != 0 {
		t.Errorf("with noise learning: passed=%v breaking=%d, want pass", on.Passed, on.Breaking)
	}

	off := Analyze(obs, Config{NoiseLearning: boolp(false)})
	if off.Passed || off.Breaking != 1 {
		t.Errorf("without noise learning: passed=%v breaking=%d, want 1 false positive", off.Passed, off.Breaking)
	}
	if off.Noise != nil {
		t.Errorf("noise must not be learned when disabled: %v", off.Noise)
	}
}

func TestNoiseDoesNotHideRealRegressions(t *testing.T) {
	obs := []Observation{{RequestID: 1, Endpoint: "/orders/{id}",
		A:         r(`{"updated_at":"t1","total":10}`),
		B:         r(`{"updated_at":"t2","total":10}`),
		Candidate: r(`{"updated_at":"t3","total":99}`)}}
	res := Analyze(obs, Config{})
	if res.Passed || res.Breaking != 1 || res.Findings[0].Path != "$.total" || res.Findings[0].RequestID != 1 {
		t.Fatalf("result = %+v, want one breaking finding on $.total", res)
	}
}

func TestNoiseIsPerEndpoint(t *testing.T) {
	obs := []Observation{
		{RequestID: 1, Endpoint: "/a", A: r(`{"ts":1}`), B: r(`{"ts":2}`), Candidate: r(`{"ts":3}`)},
		{RequestID: 2, Endpoint: "/b", A: r(`{"ts":1}`), B: r(`{"ts":1}`), Candidate: r(`{"ts":3}`)},
	}
	res := Analyze(obs, Config{})
	if res.Breaking != 1 || res.Findings[0].RequestID != 2 {
		t.Fatalf("noise on /a must not hide a change on /b: %+v", res.Findings)
	}
}

func TestFailOnControlsVerdict(t *testing.T) {
	obs := []Observation{{RequestID: 1, Endpoint: "/x",
		A: r(`{"a":1}`), B: r(`{"a":1}`), Candidate: r(`{"a":1,"new":true}`)}}

	if res := Analyze(obs, Config{}); !res.Passed || res.Additive != 1 {
		t.Errorf("additive change must not fail by default: %+v", res)
	}
	strict := Config{FailOn: []string{"breaking", "additive"}}
	if res := Analyze(obs, strict); res.Passed {
		t.Error("additive change must fail when fail_on includes additive")
	}
}

func TestLatencyRegressionIsReportedButDoesNotFailByDefault(t *testing.T) {
	cfg := Config{}
	cfg.Latency.Ratio = 1.5
	cfg.Latency.MinDeltaMS = 50
	slow := &diff.Response{Status: 200, Body: []byte(`{}`), LatencyMS: 400}
	fast := &diff.Response{Status: 200, Body: []byte(`{}`), LatencyMS: 100}
	obs := []Observation{{RequestID: 1, Endpoint: "/x", A: fast, B: fast, Candidate: slow}}

	res := Analyze(obs, cfg)
	if !res.Passed || res.Latency != 1 {
		t.Errorf("result = passed %v latency %d, want pass with 1 latency finding", res.Passed, res.Latency)
	}
	cfg.FailOn = []string{"breaking", "latency"}
	if res := Analyze(obs, cfg); res.Passed {
		t.Error("latency regression must fail when fail_on includes latency")
	}
}

func TestIncompleteObservationsAreSkipped(t *testing.T) {
	obs := []Observation{
		{RequestID: 1, Endpoint: "/x", A: r(`{}`)},
		{RequestID: 2, Endpoint: "/x", Candidate: r(`{}`)},
		{RequestID: 3, Endpoint: "/x", A: r(`{"a":1}`), Candidate: r(`{"a":1}`)},
	}
	res := Analyze(obs, Config{})
	if res.Compared != 1 || !res.Passed {
		t.Errorf("compared=%d passed=%v, want 1 and pass", res.Compared, res.Passed)
	}
}

func TestUserIgnorePathsApply(t *testing.T) {
	obs := []Observation{{RequestID: 1, Endpoint: "/x",
		A: r(`{"build":"1"}`), B: r(`{"build":"1"}`), Candidate: r(`{"build":"2"}`)}}
	if res := Analyze(obs, Config{IgnorePaths: []string{"$.build"}}); !res.Passed {
		t.Errorf("declared ignore path was not applied: %+v", res.Findings)
	}
}

func TestCandidateFailureFailsTheRun(t *testing.T) {
	obs := []Observation{{RequestID: 1, Endpoint: "/x", A: r(`{}`), B: r(`{}`), Candidate: &diff.Response{Err: "connection refused"}}}
	res := Analyze(obs, Config{})
	if res.Passed || res.Findings[0].Kind != diff.RequestFailed {
		t.Errorf("result = %+v, want request_failed", res)
	}
}

func TestBaselineUnhealthy(t *testing.T) {
	down := &diff.Response{Err: "connection refused"}
	tests := []struct {
		name string
		obs  []Observation
		want bool
	}{
		{"healthy", []Observation{{RequestID: 1, Endpoint: "/x", A: r(`{}`), Candidate: r(`{}`)}}, false},
		{"all baseline replays failed", []Observation{
			{RequestID: 1, Endpoint: "/x", A: down, Candidate: r(`{}`)},
			{RequestID: 2, Endpoint: "/x", A: down, Candidate: r(`{}`)},
		}, true},
		{"more than half failed", []Observation{
			{RequestID: 1, Endpoint: "/x", A: down, Candidate: r(`{}`)},
			{RequestID: 2, Endpoint: "/x", A: down, Candidate: r(`{}`)},
			{RequestID: 3, Endpoint: "/x", A: r(`{}`), Candidate: r(`{}`)},
		}, true},
		{"a few flaky baseline failures are tolerated", []Observation{
			{RequestID: 1, Endpoint: "/x", A: down, Candidate: r(`{}`)},
			{RequestID: 2, Endpoint: "/x", A: r(`{}`), Candidate: r(`{}`)},
			{RequestID: 3, Endpoint: "/x", A: r(`{}`), Candidate: r(`{}`)},
		}, false},
		{"nothing comparable", []Observation{{RequestID: 1, Endpoint: "/x", A: r(`{}`)}}, true},
		{"no observations", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := Analyze(tt.obs, Config{}).BaselineUnhealthy()
			if got != tt.want || (got && why == "") {
				t.Errorf("BaselineUnhealthy = %v, %q; want %v with a reason when true", got, why, tt.want)
			}
		})
	}
}
