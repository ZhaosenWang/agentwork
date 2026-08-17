package acpbackend

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractOutput: agent CLIs send rawOutput in shapes that duplicate the
// output text. opencode sends {"metadata":{"exit":0,"output":"<text>",
// "truncated":false},"output":"<text>"} — the text appears twice (nested +
// top-level). extractOutput must return ONE copy, never the doubled JSON.
func TestExtractOutput(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want string
	}{
		{
			name: "opencode-doubled-output-top-level-wins",
			raw: map[string]any{
				"metadata": map[string]any{"exit": 0, "output": "On branch main\nnothing to commit", "truncated": false},
				"output":   "On branch main\nnothing to commit",
			},
			want: "On branch main\nnothing to commit",
		},
		{
			name: "metadata-only-output-unwrapped",
			raw: map[string]any{
				"metadata": map[string]any{"exit": 1, "output": "error: bad ref", "truncated": false},
			},
			want: "error: bad ref",
		},
		{
			name: "bare-string-passes-through",
			raw:  "raw text output",
			want: "raw text output",
		},
		{
			name: "nil-empty",
			raw:  nil,
			want: "",
		},
		{
			name: "empty-output-string-falls-to-metadata",
			raw: map[string]any{
				"output":   "",
				"metadata": map[string]any{"output": "from metadata"},
			},
			want: "from metadata",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractOutput(c.raw)
			if got != c.want {
				t.Fatalf("extractOutput = %q, want %q", got, c.want)
			}
			// The doubled symptom: the want text must never appear twice.
			if c.want != "" && strings.Count(got, c.want) > 1 {
				t.Fatalf("output text appears more than once in %q", got)
			}
		})
	}
}

// TestExtractOutputUnknownShapeFallback: a rawOutput shape we don't
// recognize (no "output" key) falls back to the whole value as JSON — never
// drops output, just renders the raw structure.
func TestExtractOutputUnknownShapeFallback(t *testing.T) {
	raw := map[string]any{"result": "ok", "count": 3}
	got := extractOutput(raw)
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("fallback must be valid JSON, got %q: %v", got, err)
	}
	if back["result"] != "ok" {
		t.Fatalf("fallback lost data: %q", got)
	}
}
