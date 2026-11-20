package sanitize

import (
	"fmt"
	"strconv"
	"strings"
)

// Redacted replaces every value selected by a redaction rule.
const Redacted = "[REDACTED]"

type stepKind int

const (
	stepChild      stepKind = iota // .name or ['name']
	stepWildcard                   // .* or [*]
	stepIndex                      // [n]
	stepDescendant                 // ..name
)

type step struct {
	kind stepKind
	name string
	idx  int
}

// parsePath parses the JSONPath subset used for redaction rules:
//
//	$.a.b          child
//	$['a.b']       child with a name that contains dots
//	$.a[*]  $.a.*  every element of an array or every value of an object
//	$.a[0]         array index
//	$..name        "name" at any depth
//
// Filters, slices, unions and negative indexes are not supported. A rule must
// select something below the root, so a rule can never wipe the whole body by
// accident.
func parsePath(raw string) ([]step, error) {
	if !strings.HasPrefix(raw, "$") {
		return nil, fmt.Errorf("jsonpath %q: must start with $", raw)
	}
	var steps []step
	for i := 1; i < len(raw); {
		switch {
		case strings.HasPrefix(raw[i:], ".."):
			name, n, err := readName(raw[i+2:])
			if err != nil {
				return nil, fmt.Errorf("jsonpath %q: %w", raw, err)
			}
			steps = append(steps, step{kind: stepDescendant, name: name})
			i += 2 + n
		case raw[i] == '.':
			if i+1 < len(raw) && raw[i+1] == '*' {
				steps = append(steps, step{kind: stepWildcard})
				i += 2
				continue
			}
			name, n, err := readName(raw[i+1:])
			if err != nil {
				return nil, fmt.Errorf("jsonpath %q: %w", raw, err)
			}
			steps = append(steps, step{kind: stepChild, name: name})
			i += 1 + n
		case raw[i] == '[':
			end := strings.IndexByte(raw[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("jsonpath %q: unclosed [", raw)
			}
			s, err := parseBracket(raw[i+1 : i+end])
			if err != nil {
				return nil, fmt.Errorf("jsonpath %q: %w", raw, err)
			}
			steps = append(steps, s)
			i += end + 1
		default:
			return nil, fmt.Errorf("jsonpath %q: unexpected %q at offset %d", raw, raw[i], i)
		}
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("jsonpath %q: must select something below the root", raw)
	}
	return steps, nil
}

// readName reads a dotted name up to the next '.' or '['.
func readName(s string) (name string, n int, err error) {
	n = strings.IndexAny(s, ".[")
	if n < 0 {
		n = len(s)
	}
	name = s[:n]
	if name == "" {
		return "", 0, fmt.Errorf("empty name")
	}
	if strings.ContainsAny(name, "*]' \t") {
		return "", 0, fmt.Errorf("invalid character in name %q", name)
	}
	return name, n, nil
}

func parseBracket(inner string) (step, error) {
	switch {
	case inner == "*":
		return step{kind: stepWildcard}, nil
	case len(inner) >= 3 && inner[0] == '\'' && inner[len(inner)-1] == '\'':
		return step{kind: stepChild, name: inner[1 : len(inner)-1]}, nil
	}
	idx, err := strconv.Atoi(inner)
	if err != nil || idx < 0 {
		return step{}, fmt.Errorf("unsupported selector [%s]", inner)
	}
	return step{kind: stepIndex, idx: idx}, nil
}

// redact replaces, in place, every value in node selected by steps. Paths that
// do not match the document's shape are silently ignored: a rule written for one
// endpoint must not fail on another.
func redact(node any, steps []step) {
	if len(steps) == 0 {
		return
	}
	st, rest := steps[0], steps[1:]
	last := len(rest) == 0

	switch st.kind {
	case stepChild:
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		v, ok := m[st.name]
		if !ok {
			return
		}
		if last {
			m[st.name] = Redacted
		} else {
			redact(v, rest)
		}
	case stepWildcard:
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				if last {
					n[k] = Redacted
				} else {
					redact(v, rest)
				}
			}
		case []any:
			for i, v := range n {
				if last {
					n[i] = Redacted
				} else {
					redact(v, rest)
				}
			}
		}
	case stepIndex:
		n, ok := node.([]any)
		if !ok || st.idx >= len(n) {
			return
		}
		if last {
			n[st.idx] = Redacted
		} else {
			redact(n[st.idx], rest)
		}
	case stepDescendant:
		redactDescendant(node, st.name, rest)
	}
}

func redactDescendant(node any, name string, rest []step) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == name {
				if len(rest) == 0 {
					n[k] = Redacted
					continue // the value is gone, nothing below it to search
				}
				redact(v, rest)
			}
			redactDescendant(v, name, rest)
		}
	case []any:
		for _, v := range n {
			redactDescendant(v, name, rest)
		}
	}
}
