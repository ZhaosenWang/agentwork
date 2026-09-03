package main

import "testing"

// TestValidateMachineIDAcceptsStableTokens covers the expected orchestrator
// inputs — short visible-ASCII tokens that stay stable across sandbox
// recreations. These must all pass.
func TestValidateMachineIDAcceptsStableTokens(t *testing.T) {
	cases := []string{
		"sandbox-pool-3",
		"build-runner-1",
		"a",
		"machine_007",
		"host.example.com", // dots allowed (not in the rejected set)
		"pool-3_us-east-1",
	}
	for _, id := range cases {
		if err := validateMachineID(id); err != nil {
			t.Errorf("validateMachineID(%q) = %v, want nil", id, err)
		}
	}
}

// TestValidateMachineIDRejectsDangerousValues covers the values that would
// corrupt a downstream context: empty (no identity), path separators and "@"
// (break the "<cli>@<machine>" runtime.name split + path traversal hygiene),
// control chars and newlines (poison log lines / JSON), and overlong values
// (bloat SQLite TEXT columns + log lines). Each must fail, not propagate.
func TestValidateMachineIDRejectsDangerousValues(t *testing.T) {
	cases := []string{
		"",
		"with/slash",
		"back\\slash",
		"has@at",
		"newline\nhere",
		"carriage\rreturn",
		"tab\tchar",     // \t is a control char (< 0x20)
		"null\x00byte",  // \x00 is a control char
		"del\x7fchar",   // 0x7f is explicitly rejected
		// 129 NUL bytes: fails the control-char check before the length check;
		// included to confirm a long value still rejects even though the
		// specific reason differs. Length is exercised in its own test below.
		string(make([]byte, 129)),
	}
	for _, id := range cases {
		if err := validateMachineID(id); err == nil {
			t.Errorf("validateMachineID(%q) = nil, want error", id)
		}
	}
}

// TestValidateMachineIDBoundaryLength verifies the 128-char boundary: at the
// limit it passes, one over it fails. Guards against an off-by-one in the
// length check drifting unnoticed.
func TestValidateMachineIDBoundaryLength(t *testing.T) {
	base := make([]byte, 128)
	for i := range base {
		base[i] = 'a'
	}
	if err := validateMachineID(string(base)); err != nil {
		t.Errorf("128-char id should pass, got %v", err)
	}
	over := make([]byte, 129)
	for i := range over {
		over[i] = 'a'
	}
	if err := validateMachineID(string(over)); err == nil {
		t.Errorf("129-char id should fail, got nil")
	}
}
