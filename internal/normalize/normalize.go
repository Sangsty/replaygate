// Package normalize turns raw request paths into endpoint templates and stable
// fingerprints, so the same logical request is stored once.
package normalize

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
)

// Endpoint replaces identifier-looking path segments with {id}, so
// /users/42/orders/7 and /users/43/orders/9 group as /users/{id}/orders/{id}.
//
// A segment counts as an identifier when it is all digits, a UUID, or a hex
// string of at least 16 characters (such as a MongoDB ObjectId). This is a
// heuristic: it keeps words like "me" or "v2" intact but would also collapse a
// numeric path such as /2024/05. The query string is not part of the endpoint.
func Endpoint(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "/"
	}
	segs := strings.Split(trimmed, "/")
	for i, s := range segs {
		if isID(s) {
			segs[i] = "{id}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

func isID(s string) bool {
	switch {
	case s == "":
		return false
	case allDigits(s):
		return true
	case isUUID(s):
		return true
	case len(s) >= 16 && allHex(s):
		return true
	}
	return false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func allHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !allHex(s[i : i+1]) {
				return false
			}
		}
	}
	return true
}

// CanonicalQuery sorts query parameters by key so ?a=1&b=2 and ?b=2&a=1 are the
// same request. Values of one key keep their order, because order can matter
// (?tag=a&tag=b). An unparsable query is returned unchanged.
func CanonicalQuery(raw string) string {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	return vals.Encode()
}

// Fingerprint identifies a request by method, path, canonical query and body.
// Fields are length-prefixed so ("ab","c") and ("a","bc") cannot collide.
func Fingerprint(method, path, query string, body []byte) []byte {
	h := sha256.New()
	for _, s := range []string{method, path, CanonicalQuery(query)} {
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	h.Write(body)
	return h.Sum(nil)
}
