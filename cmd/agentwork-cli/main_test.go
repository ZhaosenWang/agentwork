package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
)

// TestGoalListURL: no limit (or non-positive) → bare /goals; positive limit
// → ?limit=N appended.
func TestGoalListURL(t *testing.T) {
	cases := []struct {
		serverURL string
		limit     int
		want      string
	}{
		{"http://127.0.0.1:7373", 0, "http://127.0.0.1:7373/goals"},
		{"http://127.0.0.1:7373", -1, "http://127.0.0.1:7373/goals"},
		{"http://127.0.0.1:7373", 1, "http://127.0.0.1:7373/goals?limit=1"},
		{"http://127.0.0.1:7373", 10, "http://127.0.0.1:7373/goals?limit=10"},
	}
	for _, c := range cases {
		if got := goalListURL(c.serverURL, c.limit); got != c.want {
			t.Fatalf("goalListURL(%q, %d) = %q, want %q", c.serverURL, c.limit, got, c.want)
		}
	}
}

// TestFilterGoalsByStatus: keeps only goals whose status matches, preserves
// order, and passes each goal object through byte-for-byte.
func TestFilterGoalsByStatus(t *testing.T) {
	goals := []json.RawMessage{
		json.RawMessage(`{"id":"a","status":"active","title":"A"}`),
		json.RawMessage(`{"id":"b","status":"done","title":"B"}`),
		json.RawMessage(`{"id":"c","status":"active","title":"C"}`),
	}
	got := filterGoalsByStatus(goals, "active")
	if len(got) != 2 {
		t.Fatalf("filtered %d goals, want 2", len(got))
	}
	if string(got[0]) != `{"id":"a","status":"active","title":"A"}` {
		t.Errorf("first goal altered: %s", got[0])
	}
	if string(got[1]) != `{"id":"c","status":"active","title":"C"}` {
		t.Errorf("second goal altered: %s", got[1])
	}
	if got := filterGoalsByStatus(goals, "nope"); len(got) != 0 {
		t.Errorf("unknown status: filtered %d goals, want 0", len(got))
	}
}

// TestGoalListStatusEndToEnd drives goalList --status against a fake daemon:
// the request hits bare /goals (no limit param) and the emitted JSON contains
// only matching goals with their full fields intact.
func TestGoalListStatusEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/goals" {
			http.NotFound(w, r)
			return
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			t.Errorf("unexpected limit param %q with --status", raw)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "g1", "title": "one", "status": "active"},
			{"id": "g2", "title": "two", "status": "done"},
			{"id": "g3", "title": "three", "status": "active"},
		})
	}))
	defer srv.Close()

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	goalList(srv.URL, []string{"--status", "active"})
	_ = pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)

	var got []map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d goals, want 2", len(got))
	}
	if got[0]["id"] != "g1" || got[1]["id"] != "g3" {
		t.Errorf("unexpected goals: %v", got)
	}
	if got[0]["title"] != "one" || got[1]["title"] != "three" {
		t.Errorf("goal fields lost: %v", got)
	}
}

// TestGoalListStatusLimitEndToEnd drives goalList --status --limit N: the
// request hits bare /goals and only the N most recent matches are emitted.
func TestGoalListStatusLimitEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/goals" {
			http.NotFound(w, r)
			return
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			t.Errorf("unexpected limit param %q with --status", raw)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "g1", "title": "one", "status": "active"},
			{"id": "g2", "title": "two", "status": "done"},
			{"id": "g3", "title": "three", "status": "active"},
		})
	}))
	defer srv.Close()

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	goalList(srv.URL, []string{"--status", "active", "--limit", "1"})
	_ = pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)

	var got []map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("got %d goals, want 1", len(got))
	}
	if got[0]["id"] != "g1" {
		t.Errorf("got goal %q, want g1", got[0]["id"])
	}
}

// TestRunsListURL: GET /goals/{id}/runs URL building.
func TestRunsListURL(t *testing.T) {
	cases := []struct {
		serverURL string
		goalID    string
		want      string
	}{
		{"http://127.0.0.1:7373", "g1", "http://127.0.0.1:7373/goals/g1/runs"},
		{"http://127.0.0.1:7373", "a/b", "http://127.0.0.1:7373/goals/a/b/runs"},
	}
	for _, c := range cases {
		if got := runsListURL(c.serverURL, c.goalID); got != c.want {
			t.Fatalf("runsListURL(%q, %q) = %q, want %q", c.serverURL, c.goalID, got, c.want)
		}
	}
}

// TestNewStatusBucket: every known status is present with a zero count.
func TestNewStatusBucket(t *testing.T) {
	b := newStatusBucket(knownGoalStatuses)
	if b.Total != 0 {
		t.Fatalf("Total = %d, want 0", b.Total)
	}
	for _, s := range knownGoalStatuses {
		if got := b.ByStatus[s]; got != 0 {
			t.Errorf("ByStatus[%q] = %d, want 0", s, got)
		}
	}
}

// TestBucketGoals: total + per-status counts over a mixed goal list,
// including an unknown status (still counted) and zero-filled known statuses.
func TestBucketGoals(t *testing.T) {
	goals := []cliGoal{
		{ID: "a", Status: "backlog"},
		{ID: "b", Status: "active"},
		{ID: "c", Status: "active"},
		{ID: "d", Status: "blocked"},
		{ID: "e", Status: "done"},
		{ID: "f", Status: "failed"},
		{ID: "g", Status: "cancelled"},
		{ID: "h", Status: "unexpected"}, // unknown status: still counted
	}
	b := bucketGoals(goals)
	if b.Total != 8 {
		t.Fatalf("Total = %d, want 8", b.Total)
	}
	want := map[string]int{
		"backlog": 1, "active": 2, "blocked": 1, "done": 1,
		"failed": 1, "cancelled": 1, "unexpected": 1,
	}
	for s, n := range want {
		if b.ByStatus[s] != n {
			t.Errorf("ByStatus[%q] = %d, want %d", s, b.ByStatus[s], n)
		}
	}
}

// TestBucketGoalsEmpty: no goals → total 0 and every known status at 0.
func TestBucketGoalsEmpty(t *testing.T) {
	b := bucketGoals(nil)
	if b.Total != 0 {
		t.Fatalf("Total = %d, want 0", b.Total)
	}
	for _, s := range knownGoalStatuses {
		if b.ByStatus[s] != 0 {
			t.Errorf("ByStatus[%q] = %d, want 0", s, b.ByStatus[s])
		}
	}
}

// TestBucketRuns: total + per-status counts over a mixed run list.
func TestBucketRuns(t *testing.T) {
	runs := []cliRun{
		{Status: "queued"},
		{Status: "running"},
		{Status: "completed"},
		{Status: "completed"},
		{Status: "failed"},
		{Status: "cancelled"},
	}
	b := bucketRuns(runs)
	if b.Total != 6 {
		t.Fatalf("Total = %d, want 6", b.Total)
	}
	want := map[string]int{
		"queued": 1, "running": 1, "completed": 2, "failed": 1, "cancelled": 1,
	}
	for _, s := range knownRunStatuses {
		if _, ok := b.ByStatus[s]; !ok {
			t.Errorf("ByStatus missing known status %q", s)
		}
	}
	for s, n := range want {
		if b.ByStatus[s] != n {
			t.Errorf("ByStatus[%q] = %d, want %d", s, b.ByStatus[s], n)
		}
	}
}

// captureStdout runs fn with os.Stdout swapped for a pipe and returns
// everything written to stdout.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = pw
	fn()
	_ = pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)
	return out
}

// TestGoalListJSONFlag: goal list accepts --json and always emits JSON (the
// default output format — the CLI's native format, since agents parse
// stdout). --json works alone or combined with --limit, which still sends
// ?limit=N to GET /goals.
func TestGoalListJSONFlag(t *testing.T) {
	var limitParam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/goals" {
			http.NotFound(w, r)
			return
		}
		limitParam = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode([]cliGoal{
			{ID: "g1", Status: "active"},
			{ID: "g2", Status: "done"},
		})
	}))
	defer srv.Close()

	cases := []struct {
		name      string
		args      []string
		wantIDs   []string
		wantLimit string
	}{
		{"--json alone", []string{"--json"}, []string{"g1", "g2"}, ""},
		{"default is JSON", nil, []string{"g1", "g2"}, ""},
		{"--json with --limit", []string{"--json", "--limit", "1"}, []string{"g1", "g2"}, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := captureStdout(t, func() { goalList(srv.URL, c.args) })
			var got []cliGoal
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("goal list output is not valid JSON: %v\n%s", err, out)
			}
			var ids []string
			for _, g := range got {
				ids = append(ids, g.ID)
			}
			if !slices.Equal(ids, c.wantIDs) {
				t.Fatalf("goal ids = %v, want %v", ids, c.wantIDs)
			}
			if limitParam != c.wantLimit {
				t.Fatalf("limit query param = %q, want %q", limitParam, c.wantLimit)
			}
		})
	}
}

// TestStatsCmdEndToEnd drives statsCmd against a fake daemon: GET /goals,
// then GET /goals/{id}/runs per goal, asserting the aggregated JSON emitted
// on stdout (goals.total, run totals + per-status counts).
func TestStatsCmdEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/goals":
			_ = json.NewEncoder(w).Encode([]cliGoal{
				{ID: "g1", Status: "active"},
				{ID: "g2", Status: "done"},
				{ID: "g3", Status: "failed"},
			})
		case "/goals/g1/runs":
			_ = json.NewEncoder(w).Encode([]cliRun{{Status: "running"}, {Status: "completed"}})
		case "/goals/g2/runs":
			_ = json.NewEncoder(w).Encode([]cliRun{{Status: "completed"}, {Status: "completed"}})
		case "/goals/g3/runs":
			_ = json.NewEncoder(w).Encode([]cliRun{{Status: "failed"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	statsCmd(srv.URL, nil)
	_ = pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)

	var got statsOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stats output is not valid JSON: %v\n%s", err, out)
	}
	if got.Goals.Total != 3 {
		t.Errorf("Goals.Total = %d, want 3", got.Goals.Total)
	}
	wantGoals := map[string]int{"backlog": 0, "active": 1, "blocked": 0, "done": 1, "failed": 1, "cancelled": 0}
	for s, n := range wantGoals {
		if got.Goals.ByStatus[s] != n {
			t.Errorf("Goals.ByStatus[%q] = %d, want %d", s, got.Goals.ByStatus[s], n)
		}
	}
	if got.Runs.Total != 5 {
		t.Errorf("Runs.Total = %d, want 5", got.Runs.Total)
	}
	wantRuns := map[string]int{"queued": 0, "running": 1, "completed": 3, "failed": 1, "cancelled": 0}
	for s, n := range wantRuns {
		if got.Runs.ByStatus[s] != n {
			t.Errorf("Runs.ByStatus[%q] = %d, want %d", s, got.Runs.ByStatus[s], n)
		}
	}
}

// TestStatsCmdEndToEndEmpty: no goals → zero-filled stats (goals and runs
// both empty; the runs endpoint is never called).
func TestStatsCmdEndToEndEmpty(t *testing.T) {
	var runsEndpointHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/goals" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		runsEndpointHits++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	statsCmd(srv.URL, nil)
	_ = pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)

	var got statsOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stats output is not valid JSON: %v\n%s", err, out)
	}
	if got.Goals.Total != 0 || got.Runs.Total != 0 {
		t.Errorf("expected empty stats, got goals.total=%d runs.total=%d", got.Goals.Total, got.Runs.Total)
	}
	if runsEndpointHits != 0 {
		t.Errorf("runs endpoint called %d times with zero goals, want 0", runsEndpointHits)
	}
}
