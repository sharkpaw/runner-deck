package runner

import (
	"net/url"
	"strings"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// githubWebBase is a variable only for testing; GitHub Enterprise is separate, and this tool currently supports only github.com
var githubWebBase = "https://github.com"

// GitHubSettingsURL builds the address of this target's Actions Runners settings page on GitHub.
//
// It points to the list page, not a single runner page such as /runners/<id>: a runner's numeric id
// is only available when querying GitHub, and deployments without a PAT don't have it at all, so the link would sometimes work and sometimes not.
// Always going to the list page is more reliable, since the runner being looked for is listed there.
//
// Returns an empty string for an invalid target, and the caller decides not to render the link. It must not return a half-built URL:
// a link that 404s on click is harder to diagnose than no link at all.
//
// Each segment is escaped, for the same reason as githubcheck.runnersEndpoint: ValidateTarget only checks / and empty values,
// and does not restrict the character set. The prefix is hardcoded to https://github.com, so the protocol cannot be rewritten by target;
// this is also why the template is allowed to put it in href.
func GitHubSettingsURL(targetType, target string) string {
	tt := strings.ToLower(strings.TrimSpace(targetType))
	raw := strings.TrimSpace(target)
	if config.ValidateTarget(tt, raw) != nil {
		return ""
	}
	if tt == "org" {
		return githubWebBase + "/organizations/" + url.PathEscape(raw) + "/settings/actions/runners"
	}
	owner, repo, ok := strings.Cut(raw, "/")
	if !ok || owner == "" || repo == "" {
		return ""
	}
	return githubWebBase + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/settings/actions/runners"
}
