// Package githubcheck queries the GitHub API to check whether a Runner is registered on GitHub,
// and deregisters it from GitHub when the Runner is deleted through this tool.
//
// Credentials come only from the Manager-side credentials directory (config/tokens/<runner name>, optional PAT),
// and are skipped when absent: this tool neither stores nor requires them.
//
// Credentials are not kept under the Runner directory: container mode mounts that whole directory into the Runner container, where jobs can read it.
// See internal/secrets. A PAT left in the old location is moved by secrets.Store before each read.
package githubcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/secrets"
)

const (
	apiTimeout = 30 * time.Second
	apiPerPage = 100 // Page size: the maximum GitHub allows
	// maxPages is a fallback that stops endless paging if the API misbehaves; 100*50 = 5000 Runners, far above real usage
	maxPages = 50
	// visibilityTTL is the minimum interval between two visibility queries for the same repository, see visibilityDue
	visibilityTTL = 24 * time.Hour
)

// errNotFound is GitHub's 404. It is a sentinel value rather than a message-text check because two callers
// judge it in opposite ways: when listing Runners, a 404 is a failed query (the target does not exist or is not visible),
// while when checking visibility, a 404 is a normal answer: "not visible", meaning "unknown whether it is public".
var errNotFound = errors.New("GitHub returned 404: the target does not exist, or the token cannot see it")

// tokenRef is a PAT together with the path it came from.
//
// The path is only there to make errors clear: on a 401 the user needs to know which file to edit, and that location
// changes with -config, so it is not a constant that can be hardcoded into the message.
type tokenRef struct {
	value string
	path  string
}

// apiBase is a variable only so tests can point it at httptest
var apiBase = "https://api.github.com"

// ghRunner matches the structure returned by the GitHub API
type ghRunner struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
	// Status is online or offline; Busy being true means GitHub is running jobs it dispatched to this runner
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

type ghRunnersResponse struct {
	TotalCount int        `json:"total_count"`
	Runners    []ghRunner `json:"runners"`
}

// Run queries each runner's status on GitHub and writes the result to .github_status.json.
//
// Two checks with different prerequisites: "is registered" needs a PAT and is skipped without one; "target repo is public"
// Works anonymously too, since most deployments have no PAT, and those are exactly the ones that most need this hint.
// A runner with neither done writes no file; in the UI it still shows as "never checked".
//
// Failing to get an answer (expired token, rate limit, network down) is not the same as "not registered", so they are recorded separately:
// the former is recorded as unknown with the reason attached, and only the latter is recorded as false. Treating the former as the latter would show
// a flat "not shown" in the UI, sending someone to troubleshoot a registration problem that doesn't exist.
func Run(cfg *config.Config, store *secrets.Store) {
	if cfg == nil || store == nil {
		return
	}
	client := &http.Client{Timeout: apiTimeout}
	now := time.Now()
	for _, item := range cfg.Runners.Items {
		installDir := item.InstallPath(cfg.Runners.BasePath)
		tok := tokenFor(store, item.Name, installDir)

		// Start from the previous conclusion and change only the fields actually re-checked this round. The whole file is written at once,
		// and without carrying over the old values, the registration check that runs every five minutes would wipe out visibility that is only checked once a day.
		prev := runner.ReadGitHubStatus(installDir)
		st := prev

		if tok.value != "" {
			st.Registered, st.Busy, st.Error = checkOne(client, tok, item.TargetType, item.Target, item.Name)
			st.LastCheck = now.Format(time.RFC3339)
		}
		if visibilityDue(item.TargetType, prev.VisibilityCheckedAt, now) {
			public, err := repoVisibility(context.Background(), client, tok, item.Target)
			if err != nil {
				// Surface it rather than silently keeping "unknown": without a PAT this is the only clue.
				log.Printf("warning: could not read the visibility of %s for runner %s: %v", item.Target, item.Name, err)
			}
			st.Public = public
			// Record the time whether or not it succeeded: visibility is not urgent, and anonymous requests get only 60 per hour,
			// so retrying immediately on failure would burn one target's quota every five minutes while it keeps failing.
			st.VisibilityCheckedAt = now.Format(time.RFC3339)
		}
		if st == prev {
			continue
		}
		_ = runner.WriteGitHubStatus(installDir, st)
	}
}

// visibilityDue reports whether this round should ask about repository visibility again.
//
// Only query repo targets: for org targets the question is whether the Runner group allows public repositories, which needs admin:org
// and mapping runners to groups, which is not the same as checking one repository here. Left for later.
//
// At most once per repository per day. Visibility almost never changes, while this check runs every five minutes and, without a PAT, uses the anonymous
// quota (60 per hour). Checking every round would drain that quota with a dozen runners,
// and drag the PAT-backed half into rate limiting too.
func visibilityDue(targetType, lastCheckedAt string, now time.Time) bool {
	if strings.ToLower(strings.TrimSpace(targetType)) != "repo" {
		return false
	}
	if lastCheckedAt == "" {
		return true
	}
	// If it can't be parsed, treat it as never checked: it is better to check once more than to trust an unreadable timestamp and never check again.
	t, err := time.Parse(time.RFC3339, lastCheckedAt)
	if err != nil {
		return true
	}
	return now.Sub(t) >= visibilityTTL
}

// repoVisibility checks whether this repository is public. nil means unknown.
//
// Send the PAT if there is one, otherwise send anonymously: visibility is public information, and most deployments have no PAT,
// which is exactly the group that most needs this hint.
//
// 404 must be recorded as "unknown", not "not public": for anonymous requests, private repos and nonexistent repos both return 404, and with a PAT lacking permission it is also 404. Writing it as false would make the UI confidently call a mistyped name
// private. That is exactly the case where the user most needs to be prompted to take a look.
// TokenForRunner reads the PAT for this runner. Returns an empty string if it is missing or empty.
func repoVisibility(ctx context.Context, client *http.Client, tok tokenRef, target string) (*bool, error) {
	if err := config.ValidateTarget("repo", target); err != nil {
		return nil, fmt.Errorf("invalid target: %w", err)
	}
	owner, repo, ok := strings.Cut(strings.TrimSpace(target), "/")
	if !ok {
		return nil, fmt.Errorf("target must be in owner/repo form")
	}
	u := apiBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)

	var data struct {
		Private bool `json:"private"`
	}
	err := doJSON(ctx, client, http.MethodGet, u, tok, &data)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	public := !data.Private
	return &public, nil
}

// Migrates once before every read, so a PAT the user placed in the runner directory per the old docs
//
// is moved out of that directory, which gets mounted into the container, within one
// check cycle (about 5 minutes) at the latest.
func TokenForRunner(store *secrets.Store, runnerName, installDir string) string {
	return tokenFor(store, runnerName, installDir).value
}

// tokenFor is the same as TokenForRunner, and also returns the path where the PAT should live, for use in error messages.
// path is always non-empty: messages must always be able to point to a location.
func tokenFor(store *secrets.Store, runnerName, installDir string) tokenRef {
	ref := tokenRef{path: secrets.PathDescription}
	if store == nil {
		return ref
	}
	// When migration fails, do not fall back to reading the old location: that is the exposure this change is meant to close. The visibility check will
	// then stall at "no credentials", so this must be surfaced, or the user only sees the check silently stop updating.
	if _, err := store.Migrate(runnerName, installDir); err != nil {
		log.Printf("warning: cannot move the PAT for runner %s out of its runner directory: %v", runnerName, err)
	}
	if p, err := store.Path(runnerName); err == nil {
		ref.path = p
	}
	ref.value = store.GitHubToken(runnerName)
	return ref
}

// checkOne returns whether this runner is registered with GitHub, and whether it is currently running a job;
// when the result cannot be determined, registered is nil and a reason is attached.
//
// busy only has a value when this runner was actually found. If it was not found, or the lookup never succeeded,
// busy is nil. That means "unknown", not "not busy". Writing the former as false would
// make the UI assert that a runner which was never looked up is idle.
func checkOne(client *http.Client, tok tokenRef, targetType, target, runnerName string) (registered, busy *bool, reason string) {
	runners, err := listRunners(context.Background(), client, tok, targetType, target)
	if err != nil {
		return nil, nil, err.Error()
	}
	found := false
	for _, r := range runners {
		if r.Name == runnerName {
			found = true
			b := r.Busy
			busy = &b
			break
		}
	}
	return &found, busy, ""
}

// listRunners lists all runners under the target, paginating automatically.
//
// Pagination is required: fetching only the first page would, for an organization with more runners than per_page, cause runners later in the list
// to be judged "not registered". That is exactly the false negative this package is meant to avoid.
func listRunners(ctx context.Context, client *http.Client, tok tokenRef, targetType, target string) ([]ghRunner, error) {
	tt := strings.ToLower(strings.TrimSpace(targetType))
	if err := config.ValidateTarget(tt, target); err != nil {
		return nil, fmt.Errorf("invalid target: %w", err)
	}
	base, err := runnersEndpoint(tt, target)
	if err != nil {
		return nil, err
	}
	var all []ghRunner
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s?per_page=%d&page=%d", base, apiPerPage, page)
		var data ghRunnersResponse
		if err := doJSON(ctx, client, http.MethodGet, u, tok, &data); err != nil {
			return nil, err
		}
		all = append(all, data.Runners...)
		// Last page: this page is not full, or total_count has been reached
		if len(data.Runners) < apiPerPage || (data.TotalCount > 0 && len(all) >= data.TotalCount) {
			return all, nil
		}
	}
	return all, fmt.Errorf("the target has more than %d runners; the list is incomplete", maxPages*apiPerPage)
}

// runnersEndpoint builds the API URL for actions/runners.
// Split each path segment: ValidateTarget only handles / and empty values and does not restrict the character set,
// so splicing an unescaped target into the URL lets a target like `myorg?x=1` hit a different endpoint.
func runnersEndpoint(targetType, target string) (string, error) {
	raw := strings.TrimSpace(target)
	if targetType == "org" {
		return apiBase + "/orgs/" + url.PathEscape(raw) + "/actions/runners", nil
	}
	owner, repo, ok := strings.Cut(raw, "/")
	if !ok {
		return "", fmt.Errorf("target must be in owner/repo form")
	}
	return apiBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/runners", nil
}

// doJSON sends an authenticated request; when out is nil the response body is not parsed (used for DELETE)
func doJSON(ctx context.Context, client *http.Client, method, u string, tok tokenRef, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return fmt.Errorf("could not build the request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// Without a PAT it sends anonymously. Registration lookups never get here (they are skipped when there are no credentials),
	// while visibility queries are deliberately allowed to be anonymous, since a public repo's visibility is already public information.
	// An empty Bearer is worse than none: GitHub treats it as a bad token and returns 401.
	if tok.value != "" {
		req.Header.Set("Authorization", "Bearer "+tok.value)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return httpError(resp, tok)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("could not parse the GitHub API response: %w", err)
	}
	return nil
}

// httpError turns status codes into actionable instructions.
// "HTTP 403" alone is not useful: rate limiting and insufficient permissions require different handling.
func httpError(resp *http.Response, tok tokenRef) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub returned 401: the token in %s is invalid or expired", tok.path)
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			reset := resp.Header.Get("X-RateLimit-Reset")
			if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return fmt.Errorf("GitHub API rate limit reached, resets in %s", time.Until(time.Unix(ts, 0)).Round(time.Second))
			}
			return fmt.Errorf("GitHub API rate limit reached, try again shortly")
		}
		return fmt.Errorf("GitHub returned 403: the token lacks scope (admin:org for an organization, repo for a repository)")
	case http.StatusNotFound:
		return errNotFound
	default:
		return fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}
}

// DeregisterResult describes the outcome of a deregistration attempt.
type DeregisterResult struct {
	// Done is true when the runner is already gone from GitHub (either deleted by this call or never existed).
	Done bool

	// MessageKey / MessageArgs describe the result; the caller renders it.
	//
	// No ready-made sentence is given here on purpose. The same result goes to two places with two language requirements: it is put into the API
	// response (which follows the request language), and it is written to the log (always English, see the i18n note in the handler).
	// A single string field cannot serve both. This package also cannot reach the translation table, which lives on the handler side
	// and is injected by main. So this only returns what happened, and the person who has the language turns it into text.
	MessageKey  string
	MessageArgs []any
}

// Deregister removes a runner from GitHub.
//
// Calls DELETE .../actions/runners/{id} with the runner's PAT. Without a PAT, deregistration is impossible:
// deleting on the GitHub side needs a PAT or a fresh removal token, and config.sh remove also needs a removal token,
// and this tool has neither. In that case, state this plainly so the user knows a runner is still left on GitHub.
//
// Still call this before deleting the install directory: the token is no longer there, but the copy that was documented to be placed there is.
// 要先被搬出来才能用上，而搬运的源头就是那个目录。
func Deregister(ctx context.Context, store *secrets.Store, installDir, targetType, target, runnerName string) DeregisterResult {
	tok := tokenFor(store, runnerName, installDir)
	if tok.value == "" {
		return DeregisterResult{
			MessageKey:  "github.dereg.no_pat",
			MessageArgs: []any{tok.path, runnerName},
		}
	}
	client := &http.Client{Timeout: apiTimeout}
	runners, err := listRunners(ctx, client, tok, targetType, target)
	if err != nil {
		return DeregisterResult{
			MessageKey:  "github.dereg.lookup_failed",
			MessageArgs: []any{err, runnerName},
		}
	}
	var id int64
	for _, r := range runners {
		if r.Name == runnerName {
			id = r.ID
			break
		}
	}
	if id == 0 {
		return DeregisterResult{Done: true, MessageKey: "github.dereg.not_listed"}
	}
	base, err := runnersEndpoint(strings.ToLower(strings.TrimSpace(targetType)), target)
	if err != nil {
		return DeregisterResult{MessageKey: "github.dereg.endpoint_failed", MessageArgs: []any{err}}
	}
	if err := doJSON(ctx, client, http.MethodDelete, base+"/"+strconv.FormatInt(id, 10), tok, nil); err != nil {
		// 404 说明它已经不在了，目的已经达到
		if errors.Is(err, errNotFound) {
			return DeregisterResult{Done: true, MessageKey: "github.dereg.not_listed"}
		}
		return DeregisterResult{
			MessageKey:  "github.dereg.delete_failed",
			MessageArgs: []any{err, runnerName},
		}
	}
	return DeregisterResult{Done: true, MessageKey: "github.dereg.done"}
}
