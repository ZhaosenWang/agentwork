package main

import "testing"

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
