package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func loginServer(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("BASIC_AUTH_USER", "admin")
	t.Setenv("BASIC_AUTH_PASSWORD", "s3cret")
	loginLimiter = &loginThrottle{}
	return newEchoServer()
}

func postLogin(h http.Handler, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getWith(h http.Handler, path, accept string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogin_BrowserIsSentToTheLoginPage(t *testing.T) {
	h := loginServer(t)
	rec := getWith(h, "/", "text/html,application/xhtml+xml")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	page := getWith(h, "/login", "text/html")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="password"`) {
		t.Fatalf("login page: code=%d", page.Code)
	}
	if rec := getWith(h, "/static/app.css", ""); rec.Code != http.StatusOK {
		t.Errorf("the login page needs its stylesheet without a session, got %d", rec.Code)
	}
}

func TestLogin_ScriptsStillGetBasicAuthChallenge(t *testing.T) {
	h := loginServer(t)
	rec := getWith(h, "/api/runners", "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("code=%d challenge=%q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	req := httptest.NewRequest(http.MethodGet, "/api/runners", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	fetched := httptest.NewRecorder()
	h.ServeHTTP(fetched, req)
	if fetched.Code != http.StatusUnauthorized || fetched.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("a page fetch must not open the browser dialog: code=%d challenge=%q",
			fetched.Code, fetched.Header().Get("WWW-Authenticate"))
	}
}

func TestLogin_WrongPasswordSetsNoCookie(t *testing.T) {
	h := loginServer(t)
	for _, tc := range [][2]string{{"admin", "wrong"}, {"root", "s3cret"}, {"", ""}} {
		rec := postLogin(h, tc[0], tc[1])
		if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%v: code=%d cookies=%v", tc, rec.Code, rec.Result().Cookies())
		}
	}
}

func TestLogin_SessionOpensTheAPIAndLogoutClearsIt(t *testing.T) {
	h := loginServer(t)
	rec := postLogin(h, "admin", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code=%d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookies=%+v", cookies)
	}
	if got := getWith(h, "/health", "", cookies[0]); got.Code != http.StatusOK {
		t.Errorf("/health = %d", got.Code)
	}
	if got := getWith(h, "/metrics", "", cookies[0]); got.Code != http.StatusOK {
		t.Errorf("a session must open protected routes, /metrics = %d", got.Code)
	}
	tampered := *cookies[0]
	tampered.Value += "0"
	if got := getWith(h, "/metrics", "", &tampered); got.Code != http.StatusUnauthorized {
		t.Errorf("tampered session accepted: %d", got.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookies[0])
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusSeeOther || out.Result().Cookies()[0].MaxAge >= 0 {
		t.Errorf("logout: code=%d cookies=%+v", out.Code, out.Result().Cookies())
	}
}

func TestSession_ExpiresAndDependsOnThePassword(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := newSessionValue("admin", "a", now)
	if !validSession(v, "admin", "a", now) {
		t.Fatal("fresh session rejected")
	}
	if validSession(v, "admin", "a", now.Add(sessionTTL)) {
		t.Error("expired session accepted")
	}
	if validSession(v, "admin", "b", now) {
		t.Error("a password change must end existing sessions")
	}
	if validSession(v, "root", "a", now) {
		t.Error("session accepted for another user")
	}
	for _, bad := range []string{"", "x", "x.y", "1.", strings.Repeat("9", 40) + ".z"} {
		if validSession(bad, "admin", "a", now) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestLogin_RepeatedFailuresAreThrottled(t *testing.T) {
	h := loginServer(t)
	for i := 0; i < loginMaxPerIP; i++ {
		postLogin(h, "admin", "wrong")
	}
	if rec := postLogin(h, "admin", "s3cret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d, want 429 even with the right password", rec.Code)
	}
	now := time.Now()
	l := &loginThrottle{}
	for i := 0; i < loginMaxOverall; i++ {
		l.fail(strings.Repeat("x", i+1), now)
	}
	if !l.blocked("fresh-address", now) {
		t.Error("changing addresses must not get around the overall limit")
	}
	if l.blocked("fresh-address", now.Add(loginWindow)) {
		t.Error("the limit must end with its window")
	}
}

func TestLoginRoutes_RedirectWhenAuthIsOff(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "")
	h := newEchoServer()
	if rec := getWith(h, "/login", "text/html"); rec.Code != http.StatusSeeOther {
		t.Errorf("GET /login = %d", rec.Code)
	}
	if rec := postLogin(h, "a", "b"); rec.Code != http.StatusSeeOther {
		t.Errorf("POST /login = %d", rec.Code)
	}
}
