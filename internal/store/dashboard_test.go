package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mustProject(t *testing.T, s *Store, ownerGitHubID int64, ownerLogin, name string) (ownerID, projectID string) {
	t.Helper()
	ctx := context.Background()
	uid, err := s.UpsertUser(ctx, ownerGitHubID, ownerLogin)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, uid, name)
	if err != nil {
		t.Fatal(err)
	}
	return uid, p.ID
}

func TestProjectAccess(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ownerA, projA := mustProject(t, s, 1, "alice", "zeta")
	_, projA2 := mustProject(t, s, 1, "alice", "alpha")
	_, projB := mustProject(t, s, 2, "bob", "bobs")
	stranger, err := s.UpsertUser(ctx, 3, "Carol")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.AddProjectMember(ctx, projB, "Carol"); err != nil { // stored lower-cased
		t.Fatal(err)
	}
	if err := s.AddProjectMember(ctx, projB, "carol"); err != nil {
		t.Fatalf("adding the same member twice must be harmless: %v", err)
	}
	for _, bad := range []string{"", "-lead", "has space", "a/b", "x'; drop table users;--"} {
		if err := s.AddProjectMember(ctx, projB, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("AddProjectMember(%q) = %v, want ErrInvalid", bad, err)
		}
	}

	list := func(uid, login string) []string {
		ps, err := s.ProjectsForUser(ctx, uid, login)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range ps {
			names = append(names, p.Name)
		}
		return names
	}
	if got := list(ownerA, "alice"); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("owner sees %v, want [alpha zeta] sorted by name", got)
	}
	if got := list(stranger, "Carol"); len(got) != 1 || got[0] != "bobs" {
		t.Errorf("member (login matched case-insensitively) sees %v, want [bobs]", got)
	}
	if got := list(stranger, "mallory"); len(got) != 0 {
		t.Errorf("a stranger sees %v", got)
	}

	for _, tt := range []struct {
		name         string
		pid, uid, lg string
		want         bool
	}{
		{"owner", projA, ownerA, "alice", true},
		{"member", projB, stranger, "CAROL", true},
		{"member of another project", projA, stranger, "carol", false},
		{"owner of another project", projB, ownerA, "alice", false},
		{"owner's second project", projA2, ownerA, "alice", true},
	} {
		got, err := s.CanAccessProject(ctx, tt.pid, tt.uid, tt.lg)
		if err != nil || got != tt.want {
			t.Errorf("%s: CanAccessProject = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

func TestListRunsNewestFirstAndScopedToProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 2)
	_, other := mustProject(t, s, 9, "other", "other")

	empty, err := s.ListRuns(ctx, other, 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, %v (want a non-nil empty slice, so JSON is [] not null)", empty, err)
	}

	var ids []string
	for _, sha := range []string{"aaaaaaa", "bbbbbbb", "ccccccc"} {
		run, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: sha, SampleSize: 10, BatchSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
		time.Sleep(5 * time.Millisecond)
	}
	got, err := s.ListRuns(ctx, pid, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ids[2] || got[1].ID != ids[1] {
		t.Errorf("runs = %+v, want newest two (%s, %s)", got, ids[2], ids[1])
	}
	if got[0].Status != "replaying" || got[0].TotalRequests != 2 {
		t.Errorf("run fields not loaded: %+v", got[0])
	}
}

func TestRunNoise(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	run, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 1, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.RunNoise(ctx, run.ID); err != nil || n == nil || len(n) != 0 {
		t.Fatalf("no noise = %#v, %v", n, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO noise_fields (run_id, endpoint, json_path) VALUES ($1::uuid,'/u/{id}','$.ts'),($1::uuid,'/a','$.etag')`, run.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.RunNoise(ctx, run.ID)
	if err != nil || len(n) != 2 || n[0].Endpoint != "/a" || n[1].JSONPath != "$.ts" {
		t.Errorf("noise = %+v, %v", n, err)
	}
}

func TestRunRequestDetail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 2)
	_, otherPID := mustProject(t, s, 9, "other", "other")
	run, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	var reqA, reqB int64
	rows, err := s.pool.Query(ctx, `SELECT id FROM recorded_requests WHERE project_id=$1::uuid ORDER BY id`, pid)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	_ = rows.Scan(&reqA)
	rows.Next()
	_ = rows.Scan(&reqB)
	rows.Close()

	// reqA has both answers and a finding; reqB only a baseline answer.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO replay_responses (run_id, request_id, variant, status, body, latency_ms) VALUES
		($1::uuid,$2,'baseline_a',200,'{"name":"x"}',5),
		($1::uuid,$2,'baseline_b',200,'{"name":"x"}',6),
		($1::uuid,$2,'candidate',200,'{"id":1}',7),
		($1::uuid,$3,'baseline_a',200,'{}',5)`, run.ID, reqA, reqB)
	exec(`INSERT INTO diffs (run_id, request_id, endpoint, severity, kind, json_path, baseline_value)
		VALUES ($1::uuid,$2,'/users/{id}','breaking','field_removed','$.name','"x"')`, run.ID, reqA)

	d, err := s.RunRequestDetail(ctx, pid, run.ID, reqA)
	if err != nil {
		t.Fatal(err)
	}
	if d.Request.Path == "" || d.Request.Method != "GET" || d.Request.Endpoint == "" {
		t.Errorf("request = %+v", d.Request)
	}
	if d.Baseline == nil || d.Baseline.Body != `{"name":"x"}` || d.Baseline.LatencyMS != 5 {
		t.Errorf("baseline = %+v (must be baseline_a, not baseline_b)", d.Baseline)
	}
	if d.Candidate == nil || d.Candidate.Body != `{"id":1}` {
		t.Errorf("candidate = %+v", d.Candidate)
	}
	if len(d.Diffs) != 1 || d.Diffs[0].JSONPath != "$.name" {
		t.Errorf("diffs = %+v", d.Diffs)
	}

	d, err = s.RunRequestDetail(ctx, pid, run.ID, reqB)
	if err != nil || d.Baseline == nil || d.Candidate != nil || d.Diffs == nil {
		t.Errorf("partial = %+v, %v (candidate must be nil, diffs a non-nil empty slice)", d, err)
	}

	for name, call := range map[string]func() error{
		"unknown request": func() error { _, e := s.RunRequestDetail(ctx, pid, run.ID, 999999); return e },
		"another project": func() error { _, e := s.RunRequestDetail(ctx, otherPID, run.ID, reqA); return e },
		"unknown run": func() error {
			_, e := s.RunRequestDetail(ctx, pid, "00000000-0000-0000-0000-000000000000", reqA)
			return e
		},
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}
