// Package gitutil holds the small git-URL helpers shared by the daemon and
// the agentwork CLI (CLI 分支 Phase 3: the machine clones/commits/pushes
// with the SAME credential embedding the daemon uses).
package gitutil

import "strings"

// CloneURL embeds the platform credentials into a clone URL, per host
// convention (the live tested shapes):
//
//	github.com → token as the username (machine-identity convention)
//	gitcode.com → oauth2:TOKEN (GitLab-style PAT)
//
// Unknown hosts fall back to token-as-username. A URL that already carries
// credentials (the owner embedded them explicitly) is left untouched; SSH
// URLs are returned as-is.
func CloneURL(gitURL, credentials string) string {
	if credentials == "" || !strings.HasPrefix(gitURL, "https://") || strings.Contains(gitURL, "@") {
		return gitURL
	}
	cred := credentials
	if strings.Contains(gitURL, "gitcode.com") {
		cred = "oauth2:" + credentials
	}
	return "https://" + cred + "@" + strings.TrimPrefix(gitURL, "https://")
}

// SanitizeURL strips embedded credentials before a URL is logged — clone
// URLs carry the platform token, and it must never reach the log file.
func SanitizeURL(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "@"); j >= 0 && !strings.Contains(rest[:j], "/") {
			return raw[:i+3] + rest[j+1:]
		}
	}
	return raw
}
