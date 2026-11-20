package sanitize

import (
	"net/http"
	"reflect"
	"testing"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    []step
		wantErr bool
	}{
		{"child", "$.a", []step{{kind: stepChild, name: "a"}}, false},
		{"nested", "$.a.b.c", []step{{kind: stepChild, name: "a"}, {kind: stepChild, name: "b"}, {kind: stepChild, name: "c"}}, false},
		{"wildcard bracket", "$.a[*]", []step{{kind: stepChild, name: "a"}, {kind: stepWildcard}}, false},
		{"wildcard dot", "$.a.*", []step{{kind: stepChild, name: "a"}, {kind: stepWildcard}}, false},
		{"array then field", "$.users[*].email", []step{{kind: stepChild, name: "users"}, {kind: stepWildcard}, {kind: stepChild, name: "email"}}, false},
		{"index", "$.items[3]", []step{{kind: stepChild, name: "items"}, {kind: stepIndex, idx: 3}}, false},
		{"quoted child", "$['user.name']", []step{{kind: stepChild, name: "user.name"}}, false},
		{"descendant", "$..email", []step{{kind: stepDescendant, name: "email"}}, false},
		{"descendant then child", "$..user.email", []step{{kind: stepDescendant, name: "user"}, {kind: stepChild, name: "email"}}, false},
		{"root array wildcard", "$[*].a", []step{{kind: stepWildcard}, {kind: stepChild, name: "a"}}, false},

		{"empty", "", nil, true},
		{"missing dollar", "a.b", nil, true},
		{"root only", "$", nil, true},
		{"trailing dot", "$.a.", nil, true},
		{"empty name", "$..", nil, true},
		{"unclosed bracket", "$.a[0", nil, true},
		{"negative index", "$.a[-1]", nil, true},
		{"slice unsupported", "$.a[1:2]", nil, true},
		{"filter unsupported", "$.a[?(@.b)]", nil, true},
		{"descendant wildcard unsupported", "$..*", nil, true},
		{"space in name", "$.a b", nil, true},
		{"empty quoted name", "$['']", nil, true},
		{"unusual name characters allowed", "$.a@b.c", []step{{kind: stepChild, name: "a@b"}, {kind: stepChild, name: "c"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if tt.wantErr || tt.want == nil {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePath(%q) = %+v, want %+v", tt.path, got, tt.want)
			}
		})
	}
}

func mustNew(t *testing.T, r Rules) *Sanitizer {
	t.Helper()
	s, err := New(r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestBodyRedaction(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		in    string
		want  string // compact JSON with sorted keys
	}{
		{
			name:  "child",
			paths: []string{"$.email"},
			in:    `{"email":"a@b.c","id":1}`,
			want:  `{"email":"[REDACTED]","id":1}`,
		},
		{
			name:  "nested child",
			paths: []string{"$.user.email"},
			in:    `{"user":{"name":"x","email":"a@b.c"}}`,
			want:  `{"user":{"email":"[REDACTED]","name":"x"}}`,
		},
		{
			name:  "wildcard over array of objects",
			paths: []string{"$.users[*].email"},
			in:    `{"users":[{"id":1,"email":"a"},{"id":2,"email":"b"}]}`,
			want:  `{"users":[{"email":"[REDACTED]","id":1},{"email":"[REDACTED]","id":2}]}`,
		},
		{
			name:  "wildcard over object values",
			paths: []string{"$.secrets.*"},
			in:    `{"ok":true,"secrets":{"a":1,"b":"x"}}`,
			want:  `{"ok":true,"secrets":{"a":"[REDACTED]","b":"[REDACTED]"}}`,
		},
		{
			name:  "wildcard over array of scalars",
			paths: []string{"$.tokens[*]"},
			in:    `{"tokens":["a","b"]}`,
			want:  `{"tokens":["[REDACTED]","[REDACTED]"]}`,
		},
		{
			name:  "index",
			paths: []string{"$.items[1]"},
			in:    `{"items":[1,2,3]}`,
			want:  `{"items":[1,"[REDACTED]",3]}`,
		},
		{
			name:  "index out of range is a no-op",
			paths: []string{"$.items[9]"},
			in:    `{"items":[1,2,3]}`,
			want:  `{"items":[1,2,3]}`,
		},
		{
			name:  "descendant at every depth",
			paths: []string{"$..email"},
			in:    `{"email":"z","a":{"email":"x","b":[{"email":"y"}]}}`,
			want:  `{"a":{"b":[{"email":"[REDACTED]"}],"email":"[REDACTED]"},"email":"[REDACTED]"}`,
		},
		{
			name:  "descendant then child",
			paths: []string{"$..user.email"},
			in:    `{"data":{"user":{"email":"a","id":1}}}`,
			want:  `{"data":{"user":{"email":"[REDACTED]","id":1}}}`,
		},
		{
			name:  "quoted name with a dot",
			paths: []string{"$['user.name']"},
			in:    `{"k":1,"user.name":"x"}`,
			want:  `{"k":1,"user.name":"[REDACTED]"}`,
		},
		{
			name:  "root array",
			paths: []string{"$[*].a"},
			in:    `[{"a":1},{"a":2}]`,
			want:  `[{"a":"[REDACTED]"},{"a":"[REDACTED]"}]`,
		},
		{
			name:  "multiple paths",
			paths: []string{"$.a", "$.b.c"},
			in:    `{"a":1,"b":{"c":2,"d":3}}`,
			want:  `{"a":"[REDACTED]","b":{"c":"[REDACTED]","d":3}}`,
		},
		{
			name:  "missing path is a no-op",
			paths: []string{"$.b.c"},
			in:    `{"a":1}`,
			want:  `{"a":1}`,
		},
		{
			name:  "type mismatch is a no-op",
			paths: []string{"$.a.b"},
			in:    `{"a":[1]}`,
			want:  `{"a":[1]}`,
		},
		{
			name:  "number text preserved",
			paths: []string{"$.x"},
			in:    `{"n":1.0,"big":12345678901234567890,"x":"y"}`,
			want:  `{"big":12345678901234567890,"n":1.0,"x":"[REDACTED]"}`,
		},
		{
			name:  "html not escaped",
			paths: []string{"$.s"},
			in:    `{"a":"<b>&","s":"x"}`,
			want:  `{"a":"<b>&","s":"[REDACTED]"}`,
		},
		{
			name:  "null value is redacted too",
			paths: []string{"$.a"},
			in:    `{"a":null}`,
			want:  `{"a":"[REDACTED]"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustNew(t, Rules{JSONPaths: tt.paths})
			got := string(s.Body([]byte(tt.in)))
			if got != tt.want {
				t.Errorf("Body(%s) with %v\n got: %s\nwant: %s", tt.in, tt.paths, got, tt.want)
			}
		})
	}
}

func TestBodyEdgeCases(t *testing.T) {
	withRules := mustNew(t, Rules{JSONPaths: []string{"$.email"}})
	noRules := mustNew(t, Rules{})

	tests := []struct {
		name string
		s    *Sanitizer
		in   string
		want string
		isNi bool // expect nil
	}{
		{"empty body", withRules, "", "", true},
		{"non-JSON dropped when rules exist", withRules, "not json", "", true},
		{"trailing garbage dropped", withRules, `{"a":1} extra`, "", true},
		{"truncated JSON dropped", withRules, `{"a":`, "", true},
		{"no rules keeps body verbatim", noRules, "not json", "not json", false},
		{"no rules keeps JSON verbatim", noRules, `{"b": 1,  "a": 2}`, `{"b": 1,  "a": 2}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.s.Body([]byte(tt.in))
			if tt.isNi {
				if got != nil {
					t.Fatalf("Body = %q, want nil", got)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("Body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBodyDoesNotMutateInput(t *testing.T) {
	s := mustNew(t, Rules{JSONPaths: []string{"$.email"}})
	in := []byte(`{"email":"a@b.c"}`)
	orig := string(in)
	_ = s.Body(in)
	if string(in) != orig {
		t.Errorf("input mutated: %s", in)
	}
}

func TestNewRejectsInvalidPath(t *testing.T) {
	if _, err := New(Rules{JSONPaths: []string{"$.ok", "$.a[1:2]"}}); err == nil {
		t.Fatal("expected error for unsupported JSONPath, got nil")
	}
}

func TestHeaders(t *testing.T) {
	in := http.Header{}
	in.Add("Authorization", "Bearer secret")
	in.Add("Cookie", "session=abc")
	in.Add("X-Internal-Token", "tok")
	in.Add("Accept", "application/json")
	in.Add("X-Multi", "a")
	in.Add("X-Multi", "b")

	tests := []struct {
		name  string
		rules Rules
		want  map[string][]string
	}{
		{
			name:  "hard floor applies with empty rules",
			rules: Rules{},
			want: map[string][]string{
				"authorization":    {Redacted},
				"cookie":           {Redacted},
				"x-internal-token": {"tok"},
				"accept":           {"application/json"},
				"x-multi":          {"a", "b"},
			},
		},
		{
			name:  "custom header is case-insensitive",
			rules: Rules{Headers: []string{"X-INTERNAL-token"}},
			want: map[string][]string{
				"authorization":    {Redacted},
				"cookie":           {Redacted},
				"x-internal-token": {Redacted},
				"accept":           {"application/json"},
				"x-multi":          {"a", "b"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustNew(t, tt.rules)
			got := s.Headers(in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Headers = %v, want %v", got, tt.want)
			}
		})
	}
	if in.Get("Authorization") != "Bearer secret" {
		t.Error("Headers modified its input")
	}
}

func TestQuery(t *testing.T) {
	tests := []struct {
		name   string
		params []string
		raw    string
		want   string
	}{
		{"no rules", nil, "a=1&token=abc", "a=1&token=abc"},
		{"redacts value keeps order", []string{"token"}, "a=1&token=abc&b=2", "a=1&token=%5BREDACTED%5D&b=2"},
		{"case-insensitive name", []string{"token"}, "Token=abc", "Token=%5BREDACTED%5D"},
		{"encoded name", []string{"token"}, "to%6Ben=abc", "to%6Ben=%5BREDACTED%5D"},
		{"repeated param", []string{"id"}, "id=1&id=2", "id=%5BREDACTED%5D&id=%5BREDACTED%5D"},
		{"param without value", []string{"token"}, "token&a=1", "token=%5BREDACTED%5D&a=1"},
		{"untouched other params", []string{"token"}, "a=1&b=2", "a=1&b=2"},
		{"empty query", []string{"token"}, "", ""},
		{"invalid escape in key stays", []string{"token"}, "to%zz=abc&a=1", "to%zz=abc&a=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustNew(t, Rules{QueryParams: tt.params})
			if got := s.Query(tt.raw); got != tt.want {
				t.Errorf("Query(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBackstop(t *testing.T) {
	h := map[string][]string{
		"Authorization":       {"Bearer x"},
		"COOKIE":              {"a=b"},
		"Set-Cookie":          {"a=b"},
		"Proxy-Authorization": {"Basic x"},
		"Accept":              {"*/*"},
	}
	Backstop(h)
	want := map[string][]string{"Accept": {"*/*"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("Backstop left %v, want %v", h, want)
	}
}

func TestDefaultRulesCompile(t *testing.T) {
	if _, err := New(DefaultRules()); err != nil {
		t.Fatalf("default rules do not compile: %v", err)
	}
}
