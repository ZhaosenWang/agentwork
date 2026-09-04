package service

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// allCodes is the single source of truth for the error codes this package
// exports. The real frontend (AI_Shell_WEB) does NOT hard-code this list —
// it looks up codes dynamically in the remote term.json, so adding a code
// only requires updating this list AND the remote term.json (via the CDN
// approval flow). The self-test frontend (web/lib/codes.ts) mirrors this
// list by hand for documentation. TestErrorCodesExported guards the
// backend list itself (uniqueness, non-empty). TestCodesMatchRemoteTermJSON
// guards that every code has a corresponding entry in the remote term.json
// — this is the cross-layer drift guard.
var allCodes = []string{
	CodeValidation,
	CodeNotFound,
	CodeDomainHasGoals,
	CodeRuntimeHasAgents,
	CodeAgentLeadsSquad,
	CodeSkillSelected,
	CodeAgentNameExists,
	CodeSkillNameExists,
	CodeRuntimeNameExists,
	CodeDomainNameExists,
	CodeGoalNotActivatable,
	CodeGoalNotReopenable,
	CodeGoalNotInReview,
	CodeSquadNameExists,
	CodeFieldRequired,
	CodeAgentHasGoals,
	CodeAgentHasSchedules,
	CodeAgentInSquads,
	CodeAgentHasRunningRuns,
	CodeAgentHandlesIssues,
	CodeDomainHasSchedules,
	CodeSquadHasGoals,
	CodeSquadHasSchedules,
	CodeSquadHandlesIssues,
	CodeScheduleBuiltIn,
}

func TestErrorCodesExported(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range allCodes {
		if c == "" {
			t.Fatal("empty code in allCodes — every entry must be a non-empty string")
		}
		if seen[c] {
			t.Errorf("duplicate error code %q — codes must be unique", c)
		}
		seen[c] = true
	}
	if len(allCodes) == 0 {
		t.Fatal("no error codes exported")
	}
}

// remoteTermURLs are the CDN-hosted localized message files. The frontend
// looks up AW.xxxxxxxx codes here for a localized message; this test asserts
// every backend code has a corresponding entry in BOTH locales so the
// frontend never gets a miss regardless of locale.
var remoteTermURLs = []string{
	"https://res-static.hc-cdn.cn/asset/locales/PROD/CloudMarket/hds/pc/zh-cn/term.json",
	"https://res-static.hc-cdn.cn/asset/locales/PROD/CloudMarket/hds/pc/en-us/term.json",
}

// TestCodesMatchRemoteTermJSON guards that every code in allCodes has a
// corresponding entry in every remote term.json (zh-cn and en-us). CI
// without network access skips (t.Skip) — the guard runs in environments
// that can reach the CDN. When a code is missing, the test reports it
// with a reminder to add it via the CDN approval flow (developers cannot
// edit term.json directly). A 10s timeout prevents the test from hanging
// indefinitely if the CDN is slow or half-open.
func TestCodesMatchRemoteTermJSON(t *testing.T) {
	client := &http.Client{Timeout: 10 * time.Second}
	for _, url := range remoteTermURLs {
		t.Run(url, func(t *testing.T) {
			resp, err := client.Get(url)
			if err != nil {
				t.Skip("cannot reach CDN — skip drift guard in offline/CI")
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Skipf("CDN returned %d — skip drift guard", resp.StatusCode)
			}
			var terms map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&terms); err != nil {
				t.Fatalf("decode term.json: %v", err)
			}
			for _, c := range allCodes {
				if _, ok := terms[c]; !ok {
					t.Errorf("code %q missing from %s — add it via the CDN approval flow", c, url)
				}
			}
		})
	}
}
