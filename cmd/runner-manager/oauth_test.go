package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/auth"
	"github.com/sharkpaw/runner-deck/internal/handler"
)

const testVerifier = "a-long-enough-pkce-verifier-for-this-test-0123456789"

func testChallenge() string {
	sum := sha256.Sum256([]byte(testVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func oauthServer(t *testing.T) http.Handler {
	t.Helper()
	orig := handler.ConfigPath
	handler.ConfigPath = ""
	t.Cleanup(func() { handler.ConfigPath = orig })
	registerLimiter = &loginThrottle{}
	return loginServer(t)
}

func send(h http.Handler, method, target, body string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func form(h http.Handler, target string, values url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return send(h, http.MethodPost, target, values.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, cookies...)
}

func signedInCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := postLogin(h, "admin", "s3cret")
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no cookie: %d", rec.Code)
	return nil
}

func authorizeValues(clientID, redirect string) url.Values {
	return url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {testChallenge()}, "code_challenge_method": {"S256"}, "state": {"xyz"},
	}
}

type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Error   string `json:"error"`
}

func signInDesktop(t *testing.T, h http.Handler, cookie *http.Cookie) tokenResponse {
	t.Helper()
	redirect := "http://127.0.0.1:5000/callback"
	values := authorizeValues(auth.DesktopClientID, redirect)
	values.Set("decision", "approve")
	rec := form(h, "/oauth/authorize", values, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") == "" {
		t.Fatalf("location = %s", loc)
	}
	rec = form(h, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "code_verifier": {testVerifier},
		"client_id": {auth.DesktopClientID}, "redirect_uri": {redirect},
	})
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil || tok.Access == "" || tok.Refresh == "" {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("tokens must not be cached")
	}
	return tok
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestMetadata_PointsAtThisServer(t *testing.T) {
	h := oauthServer(t)
	rec := send(h, http.MethodGet, "/.well-known/oauth-authorization-server", "", map[string]string{"X-Forwarded-Proto": "https"})
	var meta map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["token_endpoint"] != "https://example.com/oauth/token" {
		t.Fatalf("%v", meta)
	}
	t.Setenv("PUBLIC_URL", "https://deck.example.org/")
	rec = send(h, http.MethodGet, "/.well-known/oauth-protected-resource/mcp", "", nil)
	if !strings.Contains(rec.Body.String(), `"resource":"https://deck.example.org/mcp"`) {
		t.Fatal(rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("browser clients need CORS here")
	}
	if rec = send(h, http.MethodOptions, "/mcp", "", nil); rec.Code != http.StatusNoContent {
		t.Errorf("preflight: %d", rec.Code)
	}
}

func TestAuthorize_SendsAnonymousVisitorsToLoginAndBack(t *testing.T) {
	h := oauthServer(t)
	target := "/oauth/authorize?" + authorizeValues(auth.DesktopClientID, "http://127.0.0.1:5000/callback").Encode()
	rec := send(h, http.MethodGet, target, "", nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Location"))
	}
	next, _ := url.Parse(rec.Header().Get("Location"))
	page := send(h, http.MethodGet, next.String(), "", nil)
	if !strings.Contains(page.Body.String(), `name="next"`) {
		t.Fatal("login page lost the next address")
	}
	rec = form(h, "/login", url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {next.Query().Get("next")}})
	if rec.Header().Get("Location") != target {
		t.Fatalf("back to %s", rec.Header().Get("Location"))
	}
	cookie := rec.Result().Cookies()[0]
	consent := send(h, http.MethodGet, target, "", nil, cookie)
	if consent.Code != http.StatusOK || !strings.Contains(consent.Body.String(), "Runner Deck Desktop") || !strings.Contains(consent.Body.String(), "127.0.0.1:5000") {
		t.Fatalf("consent: %d", consent.Code)
	}
}

func TestLogin_NextCannotLeaveTheSite(t *testing.T) {
	h := oauthServer(t)
	for _, next := range []string{"https://evil.example", "//evil.example", "/oauth/authorizeX", "javascript:alert(1)"} {
		rec := form(h, "/login", url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {next}})
		if rec.Header().Get("Location") != "/" {
			t.Errorf("%s went to %s", next, rec.Header().Get("Location"))
		}
	}
}

func TestAuthorize_RefusesBadRequests(t *testing.T) {
	h := oauthServer(t)
	cookie := signedInCookie(t, h)
	// An untrusted redirect address never receives anything.
	for _, q := range []url.Values{authorizeValues("nobody", "http://127.0.0.1:1/callback"), authorizeValues(auth.DesktopClientID, "https://evil.example/cb")} {
		rec := send(h, http.MethodGet, "/oauth/authorize?"+q.Encode(), "", nil, cookie)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%v: %d %s", q, rec.Code, rec.Header().Get("Location"))
		}
	}
	base := "http://127.0.0.1:5000/callback"
	noPKCE := authorizeValues(auth.DesktopClientID, base)
	noPKCE.Set("code_challenge_method", "plain")
	wrongType := authorizeValues(auth.DesktopClientID, base)
	wrongType.Set("response_type", "token")
	for q, want := range map[string]string{noPKCE.Encode(): "invalid_request", wrongType.Encode(): "unsupported_response_type"} {
		loc, _ := url.Parse(send(h, http.MethodGet, "/oauth/authorize?"+q, "", nil, cookie).Header().Get("Location"))
		if loc.Query().Get("error") != want {
			t.Errorf("%s: %s", want, loc)
		}
	}
	deny := authorizeValues(auth.DesktopClientID, base)
	deny.Set("decision", "deny")
	loc, _ := url.Parse(form(h, "/oauth/authorize", deny, cookie).Header().Get("Location"))
	if loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Errorf("deny: %s", loc)
	}
	approve := authorizeValues(auth.DesktopClientID, base)
	approve.Set("decision", "approve")
	if rec := form(h, "/oauth/authorize", approve); rec.Code != http.StatusUnauthorized {
		t.Errorf("approve without a session: %d", rec.Code)
	}
	crossSite := send(h, http.MethodPost, "/oauth/authorize", approve.Encode(), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site",
	}, cookie)
	if crossSite.Code != http.StatusForbidden {
		t.Errorf("a cross-site approval must be refused: %d", crossSite.Code)
	}
}

func TestToken_RefreshRotatesAndBadGrantsFail(t *testing.T) {
	h := oauthServer(t)
	tok := signInDesktop(t, h, signedInCookie(t, h))
	rec := form(h, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {auth.DesktopClientID}})
	var next tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &next); err != nil || next.Refresh == "" || next.Refresh == tok.Refresh {
		t.Fatalf("refresh: %s", rec.Body)
	}
	for values, want := range map[string]string{
		url.Values{"grant_type": {"password"}}.Encode():                                                                  "unsupported_grant_type",
		url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x.y"}}.Encode():                                   "invalid_grant",
		url.Values{"grant_type": {"authorization_code"}, "client_id": {"nobody"}}.Encode():                               "invalid_client",
		url.Values{"grant_type": {"authorization_code"}, "client_id": {auth.DesktopClientID}, "code": {"nope"}}.Encode(): "invalid_grant",
	} {
		rec := send(h, http.MethodPost, "/oauth/token", values, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", want, rec.Code, rec.Body)
		}
	}
}

func TestBearer_OpensTheAPIUntilTheSessionEnds(t *testing.T) {
	h := oauthServer(t)
	cookie := signedInCookie(t, h)
	tok := signInDesktop(t, h, cookie)
	if rec := send(h, http.MethodGet, "/api/sessions", "", bearer(tok.Access)); rec.Code != http.StatusOK {
		t.Fatalf("bearer: %d", rec.Code)
	}
	if rec := send(h, http.MethodGet, "/api/sessions", "", bearer("wrong")); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("bad bearer: %d", rec.Code)
	}
	rec := send(h, http.MethodGet, "/api/sessions", "", nil, cookie)
	var list struct {
		Sessions []sessionView `json:"sessions"`
		MCPURL   string        `json:"mcp_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Sessions) != 2 || !strings.HasSuffix(list.MCPURL, "/mcp") {
		t.Fatalf("list: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "refresh_hash") {
		t.Error("the list must not carry refresh hashes")
	}
	var desktop string
	current := 0
	for _, s := range list.Sessions {
		if s.Kind == auth.KindOAuth {
			desktop = s.ID
		}
		if s.Current {
			current++
			if s.Kind != auth.KindBrowser {
				t.Error("wrong session marked current")
			}
		}
	}
	if current != 1 {
		t.Errorf("current sessions: %d", current)
	}
	if rec := send(h, http.MethodDelete, "/api/sessions/"+desktop, "", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := send(h, http.MethodDelete, "/api/sessions/"+desktop, "", nil, cookie); rec.Code != http.StatusNotFound {
		t.Errorf("second revoke: %d", rec.Code)
	}
	if rec := send(h, http.MethodGet, "/api/sessions", "", bearer(tok.Access)); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", rec.Code)
	}
}

func TestOAuthRevoke_EndsTheSession(t *testing.T) {
	h := oauthServer(t)
	tok := signInDesktop(t, h, signedInCookie(t, h))
	if rec := form(h, "/oauth/revoke", url.Values{"token": {tok.Refresh}}); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if rec := send(h, http.MethodGet, "/api/sessions", "", bearer(tok.Access)); rec.Code != http.StatusUnauthorized {
		t.Errorf("%d", rec.Code)
	}
}

func TestLogout_EndsTheSessionOnTheServer(t *testing.T) {
	h := oauthServer(t)
	cookie := signedInCookie(t, h)
	form(h, "/logout", url.Values{}, cookie)
	if rec := send(h, http.MethodGet, "/api/sessions", "", map[string]string{"Accept": "text/html"}, cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("a logged-out cookie still works: %d", rec.Code)
	}
}

func TestTicket_SignsTheAppWindowInOnce(t *testing.T) {
	h := oauthServer(t)
	tok := signInDesktop(t, h, signedInCookie(t, h))
	if rec := send(h, http.MethodPost, "/oauth/ticket", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	rec := send(h, http.MethodPost, "/oauth/ticket", "", bearer(tok.Access))
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.URL == "" {
		t.Fatalf("%s", rec.Body)
	}
	u, _ := url.Parse(out.URL)
	first := send(h, http.MethodGet, u.RequestURI(), "", nil)
	if first.Header().Get("Location") != "/" || len(first.Result().Cookies()) != 1 {
		t.Fatalf("redeem: %d %s", first.Code, first.Header().Get("Location"))
	}
	if rec := send(h, http.MethodGet, "/api/sessions", "", nil, first.Result().Cookies()[0]); rec.Code != http.StatusOK || strings.Count(rec.Body.String(), `"current":true`) != 1 {
		t.Errorf("cookie: %d %s", rec.Code, rec.Body)
	}
	if second := send(h, http.MethodGet, u.RequestURI(), "", nil); second.Header().Get("Location") != "/login" {
		t.Errorf("ticket used twice: %s", second.Header().Get("Location"))
	}
}

func TestRegister_AndRateLimit(t *testing.T) {
	h := oauthServer(t)
	json1 := map[string]string{"Content-Type": "application/json", "Origin": "https://claude.ai", "Sec-Fetch-Site": "cross-site"}
	rec := send(h, http.MethodPost, "/oauth/register", `{"client_name":"Claude <b>","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, json1)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var client struct {
		ID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &client); err != nil || client.ID == "" {
		t.Fatal(rec.Body.String())
	}
	cookie := signedInCookie(t, h)
	consent := send(h, http.MethodGet, "/oauth/authorize?"+authorizeValues(client.ID, "https://claude.ai/api/mcp/auth_callback").Encode(), "", nil, cookie)
	if strings.Contains(consent.Body.String(), "Claude <b>") || !strings.Contains(consent.Body.String(), "claude.ai") {
		t.Error("a client's name must be escaped, and its address shown")
	}
	if rec := send(h, http.MethodPost, "/oauth/register", `{"redirect_uris":["javascript:alert(1)"]}`, json1); rec.Code != http.StatusBadRequest {
		t.Errorf("bad redirect: %d", rec.Code)
	}
	if rec := send(h, http.MethodPost, "/oauth/register", `{`, json1); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: %d", rec.Code)
	}
	limited := false
	for i := 0; i < loginMaxPerIP+1; i++ {
		if send(h, http.MethodPost, "/oauth/register", `{}`, json1).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("registration is not limited")
	}
}

func mcpCall(h http.Handler, token, body string) *httptest.ResponseRecorder {
	return send(h, http.MethodPost, "/mcp", body, map[string]string{
		"Authorization": "Bearer " + token, "Content-Type": "application/json", "Origin": "https://claude.ai",
	})
}

func TestMCP_RequiresATokenAndCallsTheAPI(t *testing.T) {
	useTestConfig(t)
	h := oauthServer(t)
	useTestConfig(t)
	rec := send(h, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("anonymous: %d %v", rec.Code, rec.Header())
	}
	tok := signInDesktop(t, h, signedInCookie(t, h))
	rec = mcpCall(h, tok.Access, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	if rec = mcpCall(h, tok.Access, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); rec.Code != http.StatusAccepted {
		t.Errorf("notification: %d", rec.Code)
	}
	rec = mcpCall(h, tok.Access, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_runners"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"isError":false`) {
		t.Fatalf("tool: %d %s", rec.Code, rec.Body)
	}
	rec = mcpCall(h, tok.Access, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_runner","arguments":{"name":"missing"}}}`)
	if !strings.Contains(rec.Body.String(), `"isError":true`) {
		t.Errorf("missing runner: %s", rec.Body)
	}
	if rec = send(h, http.MethodGet, "/mcp", "", bearer(tok.Access)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestInternalMarker_CannotBeSentOverTheNetwork(t *testing.T) {
	useTestConfig(t)
	h := oauthServer(t)
	useTestConfig(t)
	if rec := send(h, http.MethodGet, "/api/runners", "", map[string]string{"X-Internal": "1", "internalKey": "true"}); rec.Code == http.StatusOK {
		t.Fatal("the API answered without credentials")
	}
}

func TestWithoutAPassword_OAuthIsOff(t *testing.T) {
	useTestConfig(t)
	t.Setenv("BASIC_AUTH_PASSWORD", "")
	orig := handler.ConfigPath
	handler.ConfigPath = ""
	t.Cleanup(func() { handler.ConfigPath = orig })
	h := newEchoServer()
	useTestConfig(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodGet, "/.well-known/oauth-protected-resource"},
		{http.MethodGet, "/oauth/authorize"},
		{http.MethodPost, "/oauth/token"},
		{http.MethodPost, "/oauth/register"},
		{http.MethodPost, "/oauth/revoke"},
		{http.MethodPost, "/oauth/ticket"},
	} {
		if rec := send(h, c.method, c.path, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", c.path, rec.Code)
		}
	}
	if rec := send(h, http.MethodGet, "/login/ticket", "", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("ticket: %d", rec.Code)
	}
	if rec := send(h, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil); rec.Code != http.StatusOK {
		t.Errorf("mcp follows the open panel: %d", rec.Code)
	}
}

func TestRedirectLabel(t *testing.T) {
	for raw, want := range map[string]string{"https://claude.ai/cb": "claude.ai", "cursor:/oauth": "cursor:", "http://[bad": "http://[bad"} {
		if got := redirectLabel(raw); got != want {
			t.Errorf("%s: %s", raw, got)
		}
	}
}

func useTestConfig(t *testing.T) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("runners:\n  base_path: "+t.TempDir()+"\n  items: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := handler.ConfigPath
	handler.ConfigPath = path
	t.Cleanup(func() { handler.ConfigPath = orig })
}

func TestCookie_RenewsWhenOld(t *testing.T) {
	h := oauthServer(t)
	cookie := signedInCookie(t, h)
	orig := cookieRenewal
	cookieRenewal = 0
	t.Cleanup(func() { cookieRenewal = orig })
	rec := send(h, http.MethodGet, "/api/sessions", "", nil, cookie)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("%d %v", rec.Code, rec.Result().Cookies())
	}
	if rec.Result().Cookies()[0].MaxAge != int(auth.SessionTTL.Seconds()) {
		t.Errorf("max age %d", rec.Result().Cookies()[0].MaxAge)
	}
	cookieRenewal = orig
	if rec := send(h, http.MethodGet, "/api/sessions", "", nil, cookie); len(rec.Result().Cookies()) != 0 {
		t.Error("a new cookie must not be reissued on every request")
	}
}

func TestAuthorizeSubmit_Rejections(t *testing.T) {
	h := oauthServer(t)
	cookie := signedInCookie(t, h)
	bad := authorizeValues("nobody", "http://127.0.0.1:1/callback")
	bad.Set("decision", "approve")
	if rec := form(h, "/oauth/authorize", bad, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown client: %d", rec.Code)
	}
	noPKCE := authorizeValues(auth.DesktopClientID, "http://127.0.0.1:1/callback")
	noPKCE.Set("decision", "approve")
	noPKCE.Set("code_challenge", "short")
	loc, _ := url.Parse(form(h, "/oauth/authorize", noPKCE, cookie).Header().Get("Location"))
	if loc.Query().Get("error") != "invalid_request" {
		t.Errorf("location %s", loc)
	}
}

func TestBufferWriter_ImplicitStatus(t *testing.T) {
	w := &bufferWriter{header: http.Header{}}
	if _, err := w.Write([]byte("x")); err != nil || w.status != http.StatusOK {
		t.Fatalf("%d %v", w.status, err)
	}
	w.WriteHeader(http.StatusTeapot)
	if w.status != http.StatusOK {
		t.Errorf("status changed to %d", w.status)
	}
	if apiStatus, _, err := apiCaller(echo.New())(context.Background(), "BAD METHOD", "/", nil); err == nil {
		t.Errorf("status %d", apiStatus)
	}
}
