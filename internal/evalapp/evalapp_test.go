package evalapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sangsty/replaygate/internal/diff"
	"github.com/Sangsty/replaygate/internal/normalize"
)

var noisePaths = []string{"$.ts", "$.request_id", "$.etag", "$.updated_at", "$.took_ms", "$.served_by"}

func get(h http.Handler, target string) diff.Response {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return diff.Response{Status: rec.Code, Body: rec.Body.Bytes()}
}

// compareAll diffs the clean build against a mutated one over the whole corpus
// with the known noise ignored, grouping findings by endpoint template.
func compareAll(t *testing.T, mutation string) map[string][]diff.Finding {
	t.Helper()
	clean, mut := New(Options{}), New(Options{Mutation: mutation})
	out := map[string][]diff.Finding{}
	for _, target := range Corpus() {
		path := strings.SplitN(target, "?", 2)[0]
		ep := normalize.Endpoint(path)
		fs := diff.Compare(get(clean, target), get(mut, target), diff.Config{IgnorePaths: noisePaths})
		out[ep] = append(out[ep], fs...)
	}
	return out
}

func TestCorpusSize(t *testing.T) {
	if n := len(Corpus()); n != 210 {
		t.Fatalf("corpus has %d requests, docs say 210", n)
	}
}

func TestCleanBuildHasOnlyDeclaredNoise(t *testing.T) {
	for ep, fs := range compareAll(t, "") {
		if len(fs) != 0 {
			t.Errorf("%s: clean vs clean differs outside the declared noise: %+v", ep, fs[0])
		}
	}
}

// Without ignoring them, the noise fields must really change between two
// responses of the same build; otherwise the evaluation would not test noise.
func TestNoiseReallyChangesOnEveryCall(t *testing.T) {
	h := New(Options{})
	for _, c := range []struct{ target, field string }{
		{"/users/3", "$.ts"}, {"/users/3", "$.request_id"}, {"/orders/3", "$.etag"}, {"/products/3", "$.updated_at"},
	} {
		fs := diff.Compare(get(h, c.target), get(h, c.target), diff.Config{})
		found := false
		for _, f := range fs {
			found = found || f.Path == c.field
		}
		if !found {
			t.Errorf("%s %s did not change between two calls: %+v", c.target, c.field, fs)
		}
	}
}

func TestEachBugProducesItsFindingAndNothingElsewhere(t *testing.T) {
	if len(Bugs) != 10 {
		t.Fatalf("want 10 seeded bugs, have %d", len(Bugs))
	}
	for _, b := range Bugs {
		t.Run(b.Name, func(t *testing.T) {
			got := compareAll(t, b.Name)
			hit := false
			for ep, fs := range got {
				if ep != b.Endpoint && len(fs) > 0 {
					t.Errorf("collateral finding on %s: %+v", ep, fs[0])
				}
				for _, f := range fs {
					if ep == b.Endpoint && f.Kind == b.Kind && strings.Contains(f.Path, b.PathHas) && f.Severity == diff.Breaking {
						hit = true
					}
				}
			}
			if !hit {
				t.Errorf("no breaking %s finding containing %q on %s; got %+v", b.Kind, b.PathHas, b.Endpoint, got[b.Endpoint])
			}
		})
	}
}

func TestBenignChangesAreNotBreaking(t *testing.T) {
	for ep, fs := range compareAll(t, "additive_field") {
		for _, f := range fs {
			if f.Severity == diff.Breaking || f.Kind != diff.FieldAdded || ep != "/users/{id}" {
				t.Errorf("additive_field produced %s %+v on %s", f.Severity, f, ep)
			}
		}
	}
	for ep, fs := range compareAll(t, "key_order") {
		if len(fs) != 0 {
			t.Errorf("key_order must be invisible to a structural diff, got %+v on %s", fs[0], ep)
		}
	}
	// ...and it really does change the bytes.
	a, b := get(New(Options{}), "/users/1"), get(New(Options{Mutation: "key_order"}), "/users/1")
	if string(a.Body) == string(b.Body) {
		t.Error("key_order did not change the serialised bytes")
	}
}

func TestRareNoiseFlipsAtRoughlyTheConfiguredRate(t *testing.T) {
	h := New(Options{RareNoise: 0.2})
	flips := 0
	const n = 2000
	for i := 0; i < n; i++ {
		if strings.Contains(string(get(h, "/products/1").Body), "node-2") {
			flips++
		}
	}
	if flips < 300 || flips > 500 { // 20% of 2000 = 400, generous bounds
		t.Errorf("flipped %d of %d, want about 400", flips, n)
	}
	if strings.Contains(string(get(New(Options{}), "/products/1").Body), "node-2") {
		t.Error("RareNoise=0 must never flip")
	}
}

func TestUnknownRoutes(t *testing.T) {
	h := New(Options{})
	for _, target := range []string{"/nope", "/users/abc", "/users/0", "/users"} {
		if r := get(h, target); r.Status != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, r.Status)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/users/1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}
