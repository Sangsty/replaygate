// Package evalapp is the system under test for the evaluation: a small JSON
// "shop" service with realistic noise, and a catalogue of seeded bugs that can
// be switched on in a candidate build.
//
// It exists so the numbers in docs/evaluation.md come from a documented,
// reproducible setup rather than from hand-picked examples. It is a synthetic
// service: results measure the gate against these bugs and this noise, not
// against every real API (see docs/evaluation.md for the limits).
package evalapp

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sangsty/replaygate/internal/diff"
)

// Options selects how a build of the service behaves. The zero value is the
// clean baseline.
type Options struct {
	// Mutation is the name of one Bug or Benign change; empty means none.
	Mutation string
	// RareNoise is the probability that /products/{id} reports served_by
	// "node-2" instead of "node-1" (an intermittent field that two replays of
	// the baseline may or may not catch changing).
	RareNoise float64
}

// Spec describes one seeded change and the finding the gate should raise.
type Spec struct {
	Name        string
	Description string
	Endpoint    string    // endpoint template of the affected route
	Kind        diff.Kind // expected finding kind
	PathHas     string    // substring the finding's JSON path must contain ("" = any)
}

// Bugs are the ten seeded regressions. Each is a different class of mistake and
// each touches only a subset of requests, like a real bug would.
var Bugs = []Spec{
	{"field_removed", "users: email dropped for every 7th id", "/users/{id}", diff.FieldRemoved, "email"},
	{"type_changed", "users: age becomes a string for every 5th id", "/users/{id}", diff.TypeChanged, "age"},
	{"value_changed", "orders: total inflated by 18% (tax applied twice)", "/orders/{id}", diff.ValueChanged, "total"},
	{"status_changed", "products: 500 instead of 200 for every 11th id", "/products/{id}", diff.StatusChanged, ""},
	{"nested_field_removed", "orders: items[1].sku dropped for every 4th id", "/orders/{id}", diff.FieldRemoved, "sku"},
	{"array_truncated", "search: last result dropped and count lowered", "/search", diff.FieldRemoved, "results"},
	{"null_introduced", "products: rating becomes null for every 6th id", "/products/{id}", diff.TypeChanged, "rating"},
	{"field_renamed", "users: name renamed to full_name", "/users/{id}", diff.FieldRemoved, "name"},
	{"precision_loss", "products: price rounded to one decimal place", "/products/{id}", diff.ValueChanged, "price"},
	{"error_on_long_query", "search: 500 when q is longer than 8 characters", "/search", diff.StatusChanged, ""},
}

// Benign changes must not fail the gate: they are the controls that show the
// gate is not simply flagging everything.
var Benign = []Spec{
	{"additive_field", "users: new optional field nickname", "/users/{id}", diff.FieldAdded, "nickname"},
	{"key_order", "all routes: keys serialised in a different order, pretty-printed", "", "", ""},
}

// Corpus returns the paths the evaluation records: 60 users, 60 orders, 60
// products and 30 searches.
func Corpus() []string {
	var out []string
	for i := 1; i <= 60; i++ {
		out = append(out, "/users/"+strconv.Itoa(i), "/orders/"+strconv.Itoa(i), "/products/"+strconv.Itoa(i))
	}
	words := []string{"red", "blue", "green", "shoe", "shirt", "jacket", "hat", "socks", "belt", "watch",
		"laptop", "phone", "tablet", "camera", "speaker", "charger", "cable", "monitor", "keyboard", "mouse",
		"headphones", "backpack", "umbrella", "sunglasses", "notebook", "stationery", "refrigerator", "microwave", "dishwasher", "thermostat"}
	for _, w := range words {
		out = append(out, "/search?q="+w)
	}
	return out
}

// New returns a handler for one build of the service.
func New(o Options) http.Handler {
	return &service{o: o, rng: mrand.New(mrand.NewSource(time.Now().UnixNano()))}
}

type service struct {
	o   Options
	mu  sync.Mutex // guards rng
	rng *mrand.Rand
	seq atomic.Int64
}

func (s *service) is(m string) bool { return s.o.Mutation == m }

func (s *service) intn(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Intn(n)
}

func (s *service) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < p
}

// nano is strictly increasing across calls, so a field built from it differs
// on every response, like a real timestamp or version counter.
func (s *service) nano() int64 { return time.Now().UnixNano() + s.seq.Add(1) }

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.write(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id := 0
	if len(parts) == 2 {
		id, _ = strconv.Atoi(parts[1])
	}
	switch {
	case len(parts) == 2 && parts[0] == "users" && id > 0:
		s.user(w, id)
	case len(parts) == 2 && parts[0] == "orders" && id > 0:
		s.order(w, id)
	case len(parts) == 2 && parts[0] == "products" && id > 0:
		s.product(w, id)
	case len(parts) == 1 && parts[0] == "search":
		s.search(w, r.URL.Query().Get("q"))
	default:
		s.write(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

var cities = []string{"Pune", "Mysuru", "Noida", "Chennai", "Kochi", "Jaipur"}

func (s *service) user(w http.ResponseWriter, id int) {
	age := 20 + id%40
	u := map[string]any{
		"id": id, "name": fmt.Sprintf("user %d", id), "email": fmt.Sprintf("user%d@example.com", id),
		"age": age, "active": id%3 != 0, "tags": []any{"a", "b"},
		"address":    map[string]any{"city": cities[id%len(cities)], "zip": fmt.Sprintf("%05d", 10000+id)},
		"ts":         s.nano(),  // noise: changes on every call
		"request_id": randHex(), // noise: random per call
	}
	switch {
	case s.is("field_removed") && id%7 == 0:
		delete(u, "email")
	case s.is("type_changed") && id%5 == 0:
		u["age"] = strconv.Itoa(age)
	case s.is("field_renamed"):
		u["full_name"] = u["name"]
		delete(u, "name")
	case s.is("additive_field"):
		u["nickname"] = fmt.Sprintf("u%d", id)
	}
	s.write(w, http.StatusOK, u)
}

func (s *service) order(w http.ResponseWriter, id int) {
	items := []any{
		map[string]any{"sku": fmt.Sprintf("SKU-%d-a", id), "qty": 1, "price": 9.99},
		map[string]any{"sku": fmt.Sprintf("SKU-%d-b", id), "qty": 2, "price": float64(id) + 0.5},
	}
	total := round(9.99+2*(float64(id)+0.5), 2)
	if s.is("value_changed") {
		total = round(total*1.18, 2)
	}
	if s.is("nested_field_removed") && id%4 == 0 {
		delete(items[1].(map[string]any), "sku")
	}
	s.write(w, http.StatusOK, map[string]any{
		"id": id, "user_id": id%60 + 1, "status": []string{"new", "paid", "shipped"}[id%3],
		"total": total, "items": items,
		"shipping": map[string]any{"method": "std", "cost": 4.5},
		"etag":     "e" + strconv.FormatInt(s.nano(), 10), // noise
	})
}

func (s *service) product(w http.ResponseWriter, id int) {
	if s.is("status_changed") && id%11 == 0 {
		s.write(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	price := round(5+float64(id)*1.37, 2)
	var rating any = round(3+float64(id%20)/10, 1)
	if s.is("precision_loss") {
		price = round(price, 1)
	}
	if s.is("null_introduced") && id%6 == 0 {
		rating = nil
	}
	served := "node-1"
	if s.chance(s.o.RareNoise) {
		served = "node-2" // intermittent noise
	}
	s.write(w, http.StatusOK, map[string]any{
		"id": id, "name": fmt.Sprintf("product %d", id), "price": price, "stock": id * 3 % 100,
		"rating": rating, "categories": []any{"c1", "c2"},
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano), // noise
		"served_by":  served,
	})
}

func (s *service) search(w http.ResponseWriter, q string) {
	if s.is("error_on_long_query") && len(q) > 8 {
		s.write(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	h := 0
	for _, c := range q {
		h = (h*31 + int(c)) % 1000
	}
	results := make([]any, 0, 5)
	for i := 0; i < 5; i++ {
		results = append(results, map[string]any{"id": h + i, "name": q + " " + strconv.Itoa(i)})
	}
	count := len(results)
	if s.is("array_truncated") {
		results = results[:4]
		count = 4
	}
	s.write(w, http.StatusOK, map[string]any{
		"query": q, "count": count, "results": results,
		"took_ms": s.intn(40) + 1, // noise
	})
}

func (s *service) write(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if s.is("key_order") {
		encode(&buf, v, true, "")
	} else {
		encode(&buf, v, false, "")
		buf.WriteByte('\n')
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// encode writes v as JSON with keys sorted (or reverse-sorted, indented when
// pretty is set), so the key_order change differs in bytes but not in meaning.
func encode(buf *bytes.Buffer, v any, pretty bool, indent string) {
	nl, in := "", ""
	if pretty {
		nl, in = "\n", indent+"  "
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if pretty {
			for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(nl + in)
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if pretty {
				buf.WriteByte(' ')
			}
			encode(buf, t[k], pretty, in)
		}
		buf.WriteString(nl + indent + "}")
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(nl + in)
			encode(buf, e, pretty, in)
		}
		buf.WriteString(nl + indent + "]")
	default:
		b, _ := json.Marshal(t)
		buf.Write(b)
	}
}
