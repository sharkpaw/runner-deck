package githubcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/secrets"
)

// fakeAPI points apiBase at a local service and returns the request paths it recorded
func fakeAPI(t *testing.T, h http.HandlerFunc) *[]string {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	orig := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = orig })
	return &seen
}

func runnersJSON(total int, names ...string) string {
	var rs []ghRunner
	for i, n := range names {
		rs = append(rs, ghRunner{ID: int64(100 + i), Name: n, OS: "linux", Status: "online"})
	}
	b, _ := json.Marshal(ghRunnersResponse{TotalCount: total, Runners: rs})
	return string(b)
}

// withToken creates a runner install directory and puts the PAT in the manager-side credentials directory.
//
// The credentials directory is deliberately kept apart from the install directory: the new location is under the config directory, which is not mounted into
// the runner container. Both share a root only so t.TempDir can clean them up.
func withToken(t *testing.T, token string) (string, *secrets.Store) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "runners", "alpha")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := &secrets.Store{Dir: filepath.Join(root, "config", "tokens")}
	if token != "" {
		if err := os.MkdirAll(store.Dir, 0700); err != nil {
			t.Fatal(err)
		}
		p, err := store.Path("alpha")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, store
}

// withLegacyToken creates an install directory and puts the PAT in the legacy location (under the runner directory),
// used to test migration: it is moved away before being read.
func withLegacyToken(t *testing.T, token string) (string, *secrets.Store) {
	t.Helper()
	dir, store := withToken(t, "")
	if err := os.WriteFile(filepath.Join(dir, secrets.LegacyRunnerTokenFile), []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

func statusOf(t *testing.T, dir string) (registered *bool, checkAt, checkErr string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, runner.GitHubStatusFile))
	if err != nil {
		return nil, "", ""
	}
	var v struct {
		Registered *bool  `json:"registered"`
		LastCheck  string `json:"last_check"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	return v.Registered, v.LastCheck, v.Error
}

// busyOf reads busy on its own, so changing the statusOf signature doesn't ripple through the long list of cases above
func busyOf(t *testing.T, dir string) *bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, runner.GitHubStatusFile))
	if err != nil {
		return nil
	}
	var v struct {
		Busy *bool `json:"busy"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	return v.Busy
}

// busyRunnersJSON is the same as runnersJSON, but marks the first runner as running a job
func busyRunnersJSON(busy bool, names ...string) string {
	var rs []ghRunner
	for i, n := range names {
		rs = append(rs, ghRunner{ID: int64(100 + i), Name: n, OS: "linux", Status: "online", Busy: busy && i == 0})
	}
	b, _ := json.Marshal(ghRunnersResponse{TotalCount: len(names), Runners: rs})
	return string(b)
}

func cfgFor(base, name, path, targetType, target string) *config.Config {
	return &config.Config{Runners: config.RunnersConfig{
		BasePath: base,
		Items:    []config.RunnerItem{{Name: name, Path: path, TargetType: targetType, Target: target}},
	}}
}

// runCfgIn makes Run land in the given install directory: base + path joins to it
func runCfgIn(installDir, name, targetType, target string) *config.Config {
	return cfgFor(filepath.Dir(installDir), name, filepath.Base(installDir), targetType, target)
}

func TestRun_RegisteredWritesTrue(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	reg, at, errMsg := statusOf(t, dir)
	if reg == nil || !*reg {
		t.Fatalf("should be recorded as registered, got %v", reg)
	}
	if at == "" {
		t.Fatal("should record the check time")
	}
	if errMsg != "" {
		t.Fatalf("there should be no error message on success, got %q", errMsg)
	}
}

func TestRun_NotRegisteredWritesFalse(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(1, "别的 runner"))
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	reg, _, _ := statusOf(t, dir)
	if reg == nil || *reg {
		t.Fatalf("target is not in the list, should be recorded as not registered, got %v", reg)
	}
}

// This is the core of this fix: failing to get an answer is not the same as not being registered.
// Writing it as false would show up in the UI as a definitive 'not shown',
// sending people to chase a registration problem that does not exist.
func TestRun_FailureIsUnknownNotUnregistered(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		headers   map[string]string
		body      string
		wantInMsg string
	}{
		{name: "令牌过期", status: http.StatusUnauthorized, wantInMsg: "401"},
		{name: "权限不足", status: http.StatusForbidden, wantInMsg: "403"},
		{name: "限流", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "0"}, wantInMsg: "rate limit"},
		{name: "目标不可见", status: http.StatusNotFound, wantInMsg: "404"},
		{name: "服务端故障", status: http.StatusBadGateway, wantInMsg: "502"},
		{name: "响应不是JSON", status: http.StatusOK, body: "not json", wantInMsg: "could not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, store := withToken(t, "pat")
			fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

			reg, at, errMsg := statusOf(t, dir)
			if reg != nil {
				t.Fatalf("when no answer can be determined it should be recorded as unknown, but was recorded as %v", *reg)
			}
			if at == "" {
				t.Fatal("the check time should be recorded even on failure, so the UI can tell 'checked but failed' apart from 'never checked'")
			}
			if !strings.Contains(errMsg, tc.wantInMsg) {
				t.Fatalf("reason %q should contain %q", errMsg, tc.wantInMsg)
			}
		})
	}
}

// Without a PAT, registration is not checked, so the UI still shows 'never checked'.
//
// The visibility half can be checked anonymously, so the state file is written out; but last_check must stay empty:
// The UI uses it to tell 'never checked' from 'checked but failed'. A time stamp here would tell a runner that was never checked
// that it was checked and failed.
func TestRun_NoTokenSkipsTheRegistrationCheck(t *testing.T) {
	dir, store := withToken(t, "")
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("should not send Authorization without a PAT, got %q", r.Header.Get("Authorization"))
		}
		_, _ = fmt.Fprint(w, `{"private":true}`)
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	for _, req := range *seen {
		if strings.Contains(req, "actions/runners") {
			t.Fatalf("should not check registration status without a token, actual requests: %v", *seen)
		}
	}
	reg, at, errMsg := statusOf(t, dir)
	if reg != nil || at != "" || errMsg != "" {
		t.Fatalf("without a token, registration status should always be empty, got %v/%q/%q", reg, at, errMsg)
	}
}

// Organization with more than one page: fetching only the first page would mark runners on later pages as unregistered
func TestRun_PaginatesBeyondFirstPage(t *testing.T) {
	dir, store := withToken(t, "pat")
	first := make([]string, apiPerPage)
	for i := range first {
		first[i] = fmt.Sprintf("filler-%d", i)
	}
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = fmt.Fprint(w, runnersJSON(apiPerPage+1, first...))
		default:
			_, _ = fmt.Fprint(w, runnersJSON(apiPerPage+1, "alpha"))
		}
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	reg, _, errMsg := statusOf(t, dir)
	if reg == nil || !*reg {
		t.Fatalf("alpha is on page two and should be marked registered, got %v (err=%q)", reg, errMsg)
	}
	if len(*seen) < 2 {
		t.Fatalf("should go to page two, actual requests: %v", *seen)
	}
}

// Stop when the first page is not full; don't waste an extra API call
func TestRun_StopsAfterShortPage(t *testing.T) {
	dir, store := withToken(t, "pat")
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)
	// Count only the runner-list requests: the same round also makes a visibility query, which hits a different endpoint
	pages := 0
	for _, req := range *seen {
		if strings.Contains(req, "actions/runners") {
			pages++
		}
	}
	if pages != 1 {
		t.Fatalf("One page is enough, pagination should not continue, actual requests: %v", *seen)
	}
}

// Visibility has three states, and the 404 case is the easiest to get wrong and the most costly:
// For anonymous requests, private repos and nonexistent repos both return 404; with a PAT, insufficient permissions also return 404.
// Recording it as false is the same as confidently calling a mistyped target 'private' -- and 'public' is exactly
// the one conclusion this check exists to announce, so getting the direction wrong is worse than saying nothing.
func TestRepoVisibility_ThreeStates(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    *bool
		wantErr string
	}{
		{name: "公开", status: 200, body: `{"private":false}`, want: ptrBool(true)},
		{name: "私有", status: 200, body: `{"private":true}`, want: ptrBool(false)},
		{name: "看不到（404）", status: 404, body: `{}`},
		{name: "GitHub 出错（500）", status: 500, body: `{}`, wantErr: "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			got, err := repoVisibility(context.Background(), &http.Client{}, tokenRef{value: "pat"}, "o/r")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("should not error, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatal("should return a reason, got nil")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("reason %q should contain %q", err, tc.wantErr)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("should be recorded as unknown, but was recorded as %v", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("should be recorded as %v, but was recorded as unknown", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("should be recorded as %v, got %v", *tc.want, *got)
			}
		})
	}
}

// Without a PAT, send anonymously: most deployments have no PAT, and they are exactly the ones that most need this hint.
// Sending an empty Bearer is worse than sending none -- GitHub treats it as a bad token and returns 401.
func TestRepoVisibility_AnonymousWhenNoToken(t *testing.T) {
	var auth string
	var sawHeader bool
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth, sawHeader = r.Header.Get("Authorization"), true
		_, _ = fmt.Fprint(w, `{"private":false}`)
	})
	got, err := repoVisibility(context.Background(), &http.Client{}, tokenRef{}, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if !sawHeader {
		t.Fatal("no request was sent")
	}
	if auth != "" {
		t.Fatalf("should not send Authorization without a PAT, got %q", auth)
	}
	if got == nil || !*got {
		t.Fatalf("private:false should be recorded as public, got %v", got)
	}
}

// Ask at most once a day per repo. This check runs every five minutes, and the anonymous quota is only 60 requests per hour,
// so asking every round would let about ten runners burn through the quota and drag the PAT-backed half into rate limiting too.
func TestRun_VisibilityIsCheckedAtMostOncePerDay(t *testing.T) {
	dir, store := withToken(t, "pat")
	visits := 0
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			visits++
			_, _ = fmt.Fprint(w, `{"private":false}`)
			return
		}
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	})
	cfg := runCfgIn(dir, "alpha", "repo", "o/r")

	Run(cfg, store)
	if visits != 1 {
		t.Fatalf("first round should check visibility once, actually %d times", visits)
	}
	if p := publicOf(t, dir); p == nil || !*p {
		t.Fatalf("private:false should be recorded as public, got %v", p)
	}

	Run(cfg, store)
	if visits != 1 {
		t.Fatalf("should not check again within 24 hours, actually %d checks in total", visits)
	}
	// The conclusion must not be wiped out in passing by the second round's registration check: the state file is written as a whole
	if p := publicOf(t, dir); p == nil || !*p {
		t.Fatalf("visibility conclusion should still be there after the second round, got %v", p)
	}

	// Move the timestamp back 25 hours so it is expired and should be checked again
	agePriorVisibilityCheck(t, dir, 25*time.Hour)
	Run(cfg, store)
	if visits != 2 {
		t.Fatalf("should be rechecked after more than 24 hours, actually %d checks in total", visits)
	}
}

// Organization targets are not handled in this round: what needs to be decided is whether the Runner group allows public repos, which requires admin:org
// It also requires mapping runners to groups, which is not the same as checking a single repo. Querying the wrong endpoint would take a 404 as the answer.
func TestRun_OrgTargetIsNotCheckedForVisibility(t *testing.T) {
	dir, store := withToken(t, "pat")
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	})
	Run(runCfgIn(dir, "alpha", "org", "acme"), store)
	for _, req := range *seen {
		if !strings.Contains(req, "actions/runners") {
			t.Fatalf("organization targets should not check repo visibility, actual requests: %v", *seen)
		}
	}
	if p := publicOf(t, dir); p != nil {
		t.Fatalf("organization target visibility should be left empty, got %v", *p)
	}
}

// When neither happens (organization target + no PAT), do not write the file: if it were written, the UI would change from 'never checked'
// to 'checked but failed', making a definite claim about a runner that was never checked.
func TestRun_OrgTargetWithoutTokenWritesNothing(t *testing.T) {
	dir, store := withToken(t, "")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not send requests when there is no PAT and visibility is not checked")
	})
	Run(runCfgIn(dir, "alpha", "org", "acme"), store)
	if _, err := os.Stat(filepath.Join(dir, runner.GitHubStatusFile)); !os.IsNotExist(err) {
		t.Fatal("should not write the state file when nothing was checked")
	}
}

// publicOf reads public separately, for the same reason as busyOf
func publicOf(t *testing.T, dir string) *bool {
	t.Helper()
	return runner.ReadGitHubStatus(dir).Public
}

func ptrBool(b bool) *bool { return &b }

// agePriorVisibilityCheck moves the visibility query time in the state file backward, to get past the 24-hour gate.
func agePriorVisibilityCheck(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	st := runner.ReadGitHubStatus(dir)
	if st.VisibilityCheckedAt == "" {
		t.Fatal("the state file has no visibility query time, so this case is meaningless")
	}
	at, err := time.Parse(time.RFC3339, st.VisibilityCheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	st.VisibilityCheckedAt = at.Add(-d).Format(time.RFC3339)
	if err := runner.WriteGitHubStatus(dir, st); err != nil {
		t.Fatal(err)
	}
}

// Special characters in the target must be escaped, otherwise the request hits another endpoint
func TestListRunners_EscapesPathSegments(t *testing.T) {
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(0))
	})
	_, err := listRunners(context.Background(), http.DefaultClient, tokenRef{value: "pat"}, "org", "my org?x=1")
	if err != nil {
		t.Fatal(err)
	}
	_, uri, _ := strings.Cut((*seen)[0], " ") // Records "METHOD URI"
	// Unescaped, `my org?x=1` turns ?x=1 into a query string and shortens the path to /orgs/my org, hitting another endpoint
	if strings.Contains(uri, " ") || strings.HasPrefix(uri, "/orgs/my org") {
		t.Fatalf("target not escaped, request hit %q", uri)
	}
	got := uri
	if !strings.Contains(got, "/orgs/my%20org%3Fx=1/actions/runners") {
		t.Fatalf("request path %q is not the expected escaped form", got)
	}
}

func TestListRunners_RejectsInvalidTarget(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made when target is invalid")
	})
	if _, err := listRunners(context.Background(), http.DefaultClient, tokenRef{value: "pat"}, "repo", "没有斜杠"); err == nil {
		t.Fatal("repo-type target lacks the owner/repo structure and should fail directly")
	}
}

// ---- Deregister ----

// Removing from this tool is not removing from GitHub: the DELETE call must actually be made
func TestDeregister_DeletesByID(t *testing.T) {
	dir, store := withToken(t, "pat")
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = fmt.Fprint(w, runnersJSON(2, "别的", "alpha"))
	})
	res := Deregister(context.Background(), store, dir, "repo", "o/r", "alpha")
	if !res.Done {
		t.Fatalf("deregistration should succeed, got %+v", res)
	}
	// alpha is the second in the list, with ID 101
	wantDelete := "DELETE /repos/o/r/actions/runners/101"
	found := false
	for _, s := range *seen {
		if s == wantDelete {
			found = true
		}
	}
	if !found {
		t.Fatalf("should send %q, actual requests: %v", wantDelete, *seen)
	}
}

// It is already absent on GitHub: the goal is already met, so it should not be reported as a failure
func TestDeregister_AbsentRunnerIsDone(t *testing.T) {
	dir, store := withToken(t, "pat")
	seen := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(1, "别的"))
	})
	res := Deregister(context.Background(), store, dir, "repo", "o/r", "alpha")
	if !res.Done {
		t.Fatalf("no Runner with the same name on GitHub should count as done, got %+v", res)
	}
	for _, s := range *seen {
		if strings.HasPrefix(s, "DELETE") {
			t.Fatalf("should not send DELETE when absent, actual requests: %v", *seen)
		}
	}
}

// Without a PAT, deregistration is impossible. This case must not be silently swallowed:
// The one left on GitHub would make a later re-add with the same name fail as a duplicate registration.
func TestDeregister_NoTokenSaysWhereToDeleteManually(t *testing.T) {
	dir, store := withToken(t, "")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made without a token")
	})
	res := Deregister(context.Background(), store, dir, "repo", "o/r", "alpha")
	if res.Done {
		t.Fatal("deregistration cannot succeed without a token")
	}
	// Assert the key and parameters, not the wording: the caller now renders the sentence per language,
	// This package only returns "what happened". The parameters must include the token file name and the Runner name --
	// Without any one of these, the message can't tell people where to delete it by hand.
	if res.MessageKey != "github.dereg.no_pat" {
		t.Fatalf("key should be github.dereg.no_pat, got %q", res.MessageKey)
	}
	wantPath, err := store.Path("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MessageArgs) != 2 || res.MessageArgs[0] != wantPath || res.MessageArgs[1] != "alpha" {
		t.Fatalf("args should be [%s alpha], got %v", wantPath, res.MessageArgs)
	}
}

func TestDeregister_APIFailureIsReported(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	res := Deregister(context.Background(), store, dir, "repo", "o/r", "alpha")
	if res.Done {
		t.Fatal("a 401 from the API must not count as a successful deregistration")
	}
	if res.MessageKey != "github.dereg.lookup_failed" {
		t.Fatalf("key should be github.dereg.lookup_failed, got %q", res.MessageKey)
	}
	if len(res.MessageArgs) != 2 || !strings.Contains(fmt.Sprint(res.MessageArgs[0]), "401") ||
		res.MessageArgs[1] != "alpha" {
		t.Fatalf("args should include the 401 and the runner name, got %v", res.MessageArgs)
	}
}

// The delete endpoint returning 404 means it is already gone, which also counts as done
func TestDeregister_DeleteReturning404IsDone(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	})
	res := Deregister(context.Background(), store, dir, "repo", "o/r", "alpha")
	if !res.Done {
		t.Fatalf("DELETE returned 404, so the runner no longer exists and should be treated as done, got %+v", res)
	}
}

// GitHub's busy field means 'this runner is running a job', so it must be stored in the state file
func TestRun_BusyRunnerIsRecorded(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, busyRunnersJSON(true, "alpha"))
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	if busy := busyOf(t, dir); busy == nil || !*busy {
		t.Fatalf("GitHub reports the runner as busy, so it should be recorded as busy, got %v", busy)
	}
}

func TestRun_IdleRunnerIsRecordedAsNotBusy(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, busyRunnersJSON(false, "alpha"))
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	if busy := busyOf(t, dir); busy == nil || *busy {
		t.Fatalf("GitHub reports the runner as not busy, so it should be recorded as idle, got %v", busy)
	}
}

// 'Runner not found' does not mean 'not busy'. Writing false would make the UI vouch for a
// runner that does not exist on GitHub at all as idle, the same mistake as the tri-state registered field.
func TestRun_BusyIsUnknownWhenRunnerAbsent(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, runnersJSON(1, "别的 runner"))
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	if busy := busyOf(t, dir); busy != nil {
		t.Fatalf("runner not found on GitHub, so busy status should be unknown, got %v", *busy)
	}
}

// The same applies when the lookup itself fails: unknown means unknown
func TestRun_BusyIsUnknownWhenCheckFails(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	if busy := busyOf(t, dir); busy != nil {
		t.Fatalf("when the lookup fails, busy status should be unknown, got %v", *busy)
	}
}

// ---- Move the old-location PAT before reading it ----

// Users who put the PAT in the runner directory, as the docs say, still need the check to use it,
// but the file must not remain in the directory mounted into the container after use.
func TestRun_MigratesLegacyTokenAndStillAuthenticates(t *testing.T) {
	dir, store := withLegacyToken(t, "legacy-pat")
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	}))
	t.Cleanup(srv.Close)
	orig := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = orig })

	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	// 1) The token at the old location was actually used
	if len(auth) == 0 {
		t.Fatal("No request was made, so the old-location PAT was not read")
	}
	if auth[0] != "Bearer legacy-pat" {
		t.Fatalf("The request should carry the old-location PAT, actual Authorization=%q", auth[0])
	}
	// 2) The old file is gone. This is the exposure this PR closes
	legacy := filepath.Join(dir, secrets.LegacyRunnerTokenFile)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("Old location %s still exists (err=%v), and the Job in the container can still read it", legacy, err)
	}
	// 3) The token was moved to the new location
	if got := store.GitHubToken("alpha"); got != "legacy-pat" {
		t.Fatalf("The new location should have the token, got %q", got)
	}
	// 4) Check results are still written out
	if reg, _, _ := statusOf(t, dir); reg == nil || !*reg {
		t.Fatalf("Should be recorded as registered, got %v", reg)
	}
}

// Deregistration when deleting a runner works the same way: the old-location PAT still works, and the old file is no longer kept after use
func TestDeregister_MigratesLegacyToken(t *testing.T) {
	dir, store := withLegacyToken(t, "legacy-pat")
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = fmt.Fprint(w, runnersJSON(1, "alpha"))
	}))
	t.Cleanup(srv.Close)
	orig := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = orig })

	res := Deregister(context.Background(), store, dir, "repo", "o/r", "alpha")
	if !res.Done {
		t.Fatalf("Should deregister successfully, got %+v", res)
	}
	for _, a := range auth {
		if a != "Bearer legacy-pat" {
			t.Fatalf("The request should carry the old-location PAT, actual %q", a)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, secrets.LegacyRunnerTokenFile)); !os.IsNotExist(err) {
		t.Fatalf("Old location still exists (err=%v)", err)
	}
}

// A 401 should point to the new location: users need to know which file to change
func TestRun_UnauthorizedNamesTheNewLocation(t *testing.T) {
	dir, store := withToken(t, "pat")
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), store)

	_, _, errMsg := statusOf(t, dir)
	wantPath, err := store.Path("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errMsg, wantPath) {
		t.Fatalf("The 401 message should point to %s, got %q", wantPath, errMsg)
	}
	if strings.Contains(errMsg, secrets.LegacyRunnerTokenFile) {
		t.Fatalf("Should no longer point users to the old file under the runner directory, got %q", errMsg)
	}
}

// Should not panic when store is nil, and should not call the API either
func TestRun_NilStoreDoesNothing(t *testing.T) {
	dir, _ := withToken(t, "pat")
	fakeAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Error("Without a store there are no credentials, so no request should be made")
	})
	Run(runCfgIn(dir, "alpha", "repo", "o/r"), nil)
	if reg, _, _ := statusOf(t, dir); reg != nil {
		t.Fatalf("Should not write out status, got %v", reg)
	}
}

func TestDeregister_NilStoreNamesTheNewLocation(t *testing.T) {
	dir, _ := withToken(t, "pat")
	res := Deregister(context.Background(), nil, dir, "repo", "o/r", "alpha")
	if res.Done {
		t.Fatal("Deregistration cannot succeed without credentials")
	}
	if res.MessageKey != "github.dereg.no_pat" {
		t.Fatalf("Should report no_pat, got %q", res.MessageKey)
	}
	if len(res.MessageArgs) != 2 || res.MessageArgs[0] != secrets.PathDescription {
		t.Fatalf("The argument should point to the generic form for the new location, got %v", res.MessageArgs)
	}
}

// TokenForRunner is this package's external read entry: read directly from the new location, and migrate the old location first, then read
func TestTokenForRunner(t *testing.T) {
	t.Run("新位置", func(t *testing.T) {
		dir, store := withToken(t, "pat")
		if got := TokenForRunner(store, "alpha", dir); got != "pat" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("旧位置先迁移", func(t *testing.T) {
		dir, store := withLegacyToken(t, "legacy-pat")
		if got := TokenForRunner(store, "alpha", dir); got != "legacy-pat" {
			t.Fatalf("got %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, secrets.LegacyRunnerTokenFile)); !os.IsNotExist(err) {
			t.Fatalf("Old file should have been moved away (err=%v)", err)
		}
	})
	t.Run("都没有时为空串", func(t *testing.T) {
		dir, store := withToken(t, "")
		if got := TokenForRunner(store, "alpha", dir); got != "" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("store 为 nil 时为空串", func(t *testing.T) {
		dir, _ := withToken(t, "pat")
		if got := TokenForRunner(nil, "alpha", dir); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}
