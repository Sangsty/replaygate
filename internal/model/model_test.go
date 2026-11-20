package model

import (
	"strings"
	"testing"
)

func TestRecordedRequestValidate(t *testing.T) {
	valid := RecordedRequest{Method: "GET", Path: "/users/1", Query: "a=1", Status: 200, LatencyMS: 5}

	tests := []struct {
		name    string
		mutate  func(r *RecordedRequest)
		wantErr string // substring; empty means valid
	}{
		{"valid", func(r *RecordedRequest) {}, ""},
		{"valid empty query and body", func(r *RecordedRequest) { r.Query = "" }, ""},
		{"post rejected", func(r *RecordedRequest) { r.Method = "POST" }, "method must be GET"},
		{"lowercase method rejected", func(r *RecordedRequest) { r.Method = "get" }, "method must be GET"},
		{"relative path", func(r *RecordedRequest) { r.Path = "users" }, "path must start"},
		{"empty path", func(r *RecordedRequest) { r.Path = "" }, "path must start"},
		{"path too long", func(r *RecordedRequest) { r.Path = "/" + strings.Repeat("a", MaxPathLen) }, "path longer"},
		{"query too long", func(r *RecordedRequest) { r.Query = strings.Repeat("a", MaxQueryLen+1) }, "query longer"},
		{"body too large", func(r *RecordedRequest) { r.Body = make([]byte, MaxBodyBytes+1) }, "body larger"},
		{"body at limit ok", func(r *RecordedRequest) { r.Body = make([]byte, MaxBodyBytes) }, ""},
		{"status too low", func(r *RecordedRequest) { r.Status = 99 }, "out of range"},
		{"status too high", func(r *RecordedRequest) { r.Status = 600 }, "out of range"},
		{"negative latency", func(r *RecordedRequest) { r.LatencyMS = -1 }, "latency_ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)
			err := r.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}
