package issue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Provider abstracts a Git-hosting platform's issue API (M4-B): the adapter
// must not be GitHub-only — GitCode, GitLab, Gitee etc. speak different REST
// shapes and webhook signatures. The domain config selects the provider; the
// rest of the platform (poller, closer, comment API, webhook) talks to the
// interface only.
type Provider interface {
	ListOpenIssues(ctx context.Context, repo string) ([]Issue, error)
	ListComments(ctx context.Context, repo string, number int) ([]Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) error
	CloseIssue(ctx context.Context, repo string, number int) error
}

// NewProvider builds the provider named by the domain's issue_provider
// setting ("github" | "gitcode").
func NewProvider(name, token string) (Provider, error) {
	switch name {
	case "", "github":
		return NewClient(token), nil
	case "gitcode":
		return NewGitCodeClient(token), nil
	default:
		return nil, fmt.Errorf("unknown issue provider %q (github|gitcode)", name)
	}
}

// SourceRef encodes an issue as the goal's source_ref, provider-qualified so
// the closer/comment paths can route back to the right API.
func SourceRef(provider, repo string, number int) string {
	return provider + ":" + repo + "#" + strconv.Itoa(number)
}

// ParseSourceRef decodes a source_ref ("github:owner/repo#123" /
// "gitcode:owner/repo#123"); ok=false for anything else.
func ParseSourceRef(ref string) (provider, repo string, number int, ok bool) {
	rest, found := strings.CutPrefix(ref, "github:")
	if !found {
		rest, found = strings.CutPrefix(ref, "gitcode:")
		if !found {
			return "", "", 0, false
		}
		provider = "gitcode"
	} else {
		provider = "github"
	}
	repoPart, numPart, found := strings.Cut(rest, "#")
	if !found || repoPart == "" {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return "", "", 0, false
	}
	return provider, repoPart, n, true
}
