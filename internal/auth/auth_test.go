package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const verifier = "a-long-enough-pkce-verifier-for-this-test-0123456789"

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open("", "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now()}
	s.now = c.now
	return s, c
}

func signIn(t *testing.T, s *Store) Tokens {
	t.Helper()
	code, err := s.NewCode(DesktopClientID, "http://127.0.0.1:5000/callback", challenge(verifier))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.ExchangeCode(code, verifier, DesktopClientID, "http://127.0.0.1:5000/callback", Meta{IP: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestExchange_RequiresTheRightVerifierAndIsSingleUse(t *testing.T) {
	s, _ := newStore(t)
	redirect := "http://127.0.0.1:5000/callback"
	code, _ := s.NewCode(DesktopClientID, redirect, challenge(verifier))
	if _, err := s.ExchangeCode(code, "wrong", DesktopClientID, redirect, Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("wrong verifier: %v", err)
	}
	if _, err := s.ExchangeCode(code, verifier, DesktopClientID, redirect, Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("a failed attempt must spend the code: %v", err)
	}
	code, _ = s.NewCode(DesktopClientID, redirect, challenge(verifier))
	if _, err := s.ExchangeCode(code, verifier, DesktopClientID, "http://127.0.0.1:6000/callback", Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("another redirect: %v", err)
	}
	if _, err := s.ExchangeCode("x", verifier, "nobody", redirect, Meta{}); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("unknown client: %v", err)
	}
	if _, err := s.NewCode(DesktopClientID, redirect, "short"); err == nil {
		t.Fatal("a code needs PKCE")
	}
}

func TestExchange_ExpiredCode(t *testing.T) {
	s, c := newStore(t)
	redirect := "http://127.0.0.1:5000/callback"
	code, _ := s.NewCode(DesktopClientID, redirect, challenge(verifier))
	c.t = c.t.Add(codeTTL)
	if _, err := s.ExchangeCode(code, verifier, DesktopClientID, redirect, Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatal(err)
	}
}

func TestAccessToken_ExpiresAndFollowsTheSession(t *testing.T) {
	s, c := newStore(t)
	tok := signIn(t, s)
	if _, err := s.VerifyAccess(tok.Access); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyCookie(tok.Access); err == nil {
		t.Fatal("an access token is not a cookie")
	}
	c.t = c.t.Add(AccessTTL + time.Second)
	if _, err := s.VerifyAccess(tok.Access); err == nil {
		t.Fatal("expired token accepted")
	}
	c.t = c.t.Add(-AccessTTL)
	if ok, err := s.Revoke(tok.SessionID); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if _, err := s.VerifyAccess(tok.Access); err == nil {
		t.Fatal("a revoked session's token accepted")
	}
	if ok, _ := s.Revoke("missing"); ok {
		t.Fatal("unknown session reported as revoked")
	}
}

func TestVerify_RejectsForgedTokens(t *testing.T) {
	s, _ := newStore(t)
	tok := signIn(t, s)
	other, _ := Open("", "admin", "pw")
	forged := other.sign(tok.SessionID, "access", time.Hour, time.Now())
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sid": tok.SessionID, "typ": "access", "aud": audience}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	for _, bad := range []string{forged, none, "", "a.b.c"} {
		if _, err := s.VerifyAccess(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRefresh_RotatesExtendsAndDetectsReuse(t *testing.T) {
	s, c := newStore(t)
	first := signIn(t, s)
	c.t = c.t.Add(170 * 24 * time.Hour)
	second, err := s.Refresh(first.Refresh, DesktopClientID, Meta{IP: "5.6.7.8"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Refresh == first.Refresh {
		t.Fatal("refresh token did not rotate")
	}
	got := s.Sessions()
	if len(got) != 1 || got[0].IP != "5.6.7.8" || got[0].ExpiresAt.Sub(c.t) != SessionTTL {
		t.Fatalf("sessions = %+v", got)
	}
	if got[0].RefreshHash != "" || got[0].PrevRefreshHash != "" {
		t.Fatal("a listing must not carry refresh hashes")
	}
	// A retry after a lost response is fine shortly after rotation.
	third, err := s.Refresh(first.Refresh, DesktopClientID, Meta{})
	if err != nil {
		t.Fatalf("retry within grace: %v", err)
	}
	c.t = c.t.Add(refreshGrace)
	if _, err := s.Refresh(second.Refresh, DesktopClientID, Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("late reuse: %v", err)
	}
	if _, err := s.Refresh(third.Refresh, DesktopClientID, Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("reuse must end the whole session: %v", err)
	}
	if len(s.Sessions()) != 0 {
		t.Fatal("session survived reuse")
	}
}

func TestRefresh_Rejections(t *testing.T) {
	s, c := newStore(t)
	tok := signIn(t, s)
	for _, bad := range []string{"nodot", tok.SessionID + ".wrong", "missing.x"} {
		if _, err := s.Refresh(bad, DesktopClientID, Meta{}); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := s.Refresh(tok.Refresh, "someone-else", Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("other client: %v", err)
	}
	c.t = c.t.Add(SessionTTL)
	if _, err := s.Refresh(tok.Refresh, DesktopClientID, Meta{}); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("expired: %v", err)
	}
}

func TestRevokeRefresh(t *testing.T) {
	s, _ := newStore(t)
	tok := signIn(t, s)
	for _, unknown := range []string{"nodot", tok.SessionID + ".wrong"} {
		if err := s.RevokeRefresh(unknown); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Sessions()) != 1 {
		t.Fatal("a wrong token must not end the session")
	}
	if err := s.RevokeRefresh(tok.Refresh); err != nil {
		t.Fatal(err)
	}
	if len(s.Sessions()) != 0 {
		t.Fatal("session not ended")
	}
}

func TestBrowserSession_LastsAndRenews(t *testing.T) {
	s, c := newStore(t)
	cookie, err := s.StartBrowserSession(Meta{UserAgent: strings.Repeat("x", 500), IP: "9.9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(SessionTTL - time.Hour)
	v, err := s.VerifyCookie(cookie)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.RenewCookie(v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(SessionTTL - time.Hour)
	if _, err := s.VerifyCookie(renewed); err != nil {
		t.Fatalf("renewed cookie: %v", err)
	}
	if _, err := s.VerifyCookie(cookie); err == nil {
		t.Fatal("old cookie outlived its expiry")
	}
	if len(s.Sessions()[0].UserAgent) != 300 {
		t.Error("user agent not limited")
	}
	if _, err := s.RenewCookie("missing"); err == nil {
		t.Error("renewed a missing session")
	}
	c.t = c.t.Add(SessionTTL)
	if len(s.Sessions()) != 0 {
		t.Error("expired session listed")
	}
}

func TestTicket_OneTimeAndShortLived(t *testing.T) {
	s, c := newStore(t)
	tok := signIn(t, s)
	ticket, err := s.NewTicket(tok.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := s.RedeemTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.VerifyCookie(cookie); err != nil || v.SessionID != tok.SessionID {
		t.Fatalf("cookie: %+v %v", v, err)
	}
	if _, err := s.RedeemTicket(ticket); err == nil {
		t.Fatal("ticket used twice")
	}
	ticket, _ = s.NewTicket(tok.SessionID)
	c.t = c.t.Add(ticketTTL)
	if _, err := s.RedeemTicket(ticket); err == nil {
		t.Fatal("expired ticket accepted")
	}
}

func TestRegister_ValidatesRedirects(t *testing.T) {
	s, _ := newStore(t)
	for _, bad := range [][]string{nil, {"javascript:alert(1)"}, {"http://evil.example/cb"}, {"https://a/cb#frag"}, {"data:text/html,x"}, {"nope"}} {
		if _, err := s.Register("x", bad); !errors.Is(err, ErrBadRedirect) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	c, err := s.Register("  ", []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost:3000/cb", "cursor://anysphere.cursor/oauth"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "MCP" {
		t.Errorf("name = %q", c.Name)
	}
	found, ok := s.FindClient(c.ID)
	if !ok {
		t.Fatal("not found")
	}
	for raw, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"https://claude.ai/other":                 false,
		"http://localhost:9999/cb":                true,
		"http://localhost:9999/other":             false,
		"http://evil.example:3000/cb":             false,
		"cursor://anysphere.cursor/oauth":         true,
	} {
		if got := found.RedirectAllowed(raw); got != want {
			t.Errorf("%s: %v", raw, got)
		}
	}
	if _, ok := s.FindClient("missing"); ok {
		t.Error("found a missing client")
	}
	for i := 0; i < maxClients+5; i++ {
		if _, err := s.Register("c", []string{"https://a/cb"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.data.Clients) != maxClients {
		t.Errorf("clients = %d", len(s.data.Clients))
	}
}

func TestDesktopClient_AnyLoopbackPort(t *testing.T) {
	c, ok := (&Store{}).FindClient(DesktopClientID)
	if !ok || !c.RedirectAllowed("http://127.0.0.1:49152/callback") || c.RedirectAllowed("http://example.com/callback") {
		t.Fatalf("%+v", c)
	}
}

func TestOpen_PersistsAndPasswordChangeEndsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s, err := Open(path, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	tok := signIn(t, s)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok.Refresh) {
		t.Error("refresh token stored in the clear")
	}
	again, err := Open(path, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.VerifyAccess(tok.Access); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	if _, err := again.Refresh(tok.Refresh, DesktopClientID, Meta{}); err != nil {
		t.Fatalf("refresh after a restart: %v", err)
	}
	changed, _ := Open(path, "admin", "new")
	if _, err := changed.VerifyAccess(tok.Access); err == nil {
		t.Error("access token survived a password change")
	}
	if _, err := changed.Refresh(tok.Refresh, DesktopClientID, Meta{}); err == nil {
		t.Error("refresh token survived a password change")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "admin", "pw"); err == nil {
		t.Error("corrupt file accepted")
	}
}

func TestOpen_ReportsAFileItCannotUse(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, "admin", "pw"); err == nil {
		t.Error("a directory is not a store")
	}
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocked, "auth.json"), "admin", "pw"); err == nil {
		t.Error("a path under a file cannot be written")
	}
}

func TestWrites_ReportAStoreThatCannotBeSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := s.StartBrowserSession(Meta{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.VerifyCookie(cookie)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := s.NewCode(DesktopClientID, "http://127.0.0.1:1/callback", challenge(verifier))
	s.path = filepath.Join(path, "not-a-directory", "auth.json")
	if _, err := s.StartBrowserSession(Meta{}); err == nil {
		t.Error("browser session")
	}
	if _, err := s.RenewCookie(v.SessionID); err == nil {
		t.Error("renew")
	}
	if _, err := s.ExchangeCode(code, verifier, DesktopClientID, "http://127.0.0.1:1/callback", Meta{}); err == nil {
		t.Error("exchange")
	}
}

func TestExpiredCodesAndTicketsAreForgotten(t *testing.T) {
	s, c := newStore(t)
	tok := signIn(t, s)
	_, _ = s.NewCode(DesktopClientID, "http://127.0.0.1:1/callback", challenge(verifier))
	_, _ = s.NewTicket(tok.SessionID)
	c.t = c.t.Add(time.Hour)
	_, _ = s.NewCode(DesktopClientID, "http://127.0.0.1:1/callback", challenge(verifier))
	_, _ = s.NewTicket(tok.SessionID)
	if len(s.codes) != 1 || len(s.tickets) != 1 {
		t.Fatalf("codes %d, tickets %d", len(s.codes), len(s.tickets))
	}
}

func TestVerify_RecordsUseOccasionally(t *testing.T) {
	s, c := newStore(t)
	tok := signIn(t, s)
	used := c.t
	c.t = c.t.Add(touchEvery - time.Second)
	if _, err := s.VerifyAccess(tok.Access); err != nil {
		t.Fatal(err)
	}
	if !s.Sessions()[0].LastUsedAt.Equal(used) {
		t.Error("recorded too soon")
	}
	c.t = c.t.Add(time.Second)
	if _, err := s.VerifyAccess(tok.Access); err != nil {
		t.Fatal(err)
	}
	if s.Sessions()[0].LastUsedAt.Equal(used) {
		t.Error("not recorded")
	}
}

func TestSessions_MostRecentFirst(t *testing.T) {
	s, c := newStore(t)
	_ = signIn(t, s)
	c.t = c.t.Add(time.Minute)
	second := signIn(t, s)
	if got := s.Sessions(); got[0].ID != second.SessionID {
		t.Fatalf("%+v", got)
	}
}

func TestRedirectAllowed_BadURL(t *testing.T) {
	c := Client{RedirectURIs: []string{"https://a/cb", "http://[bad"}}
	if c.RedirectAllowed("http://[bad:1/") || c.RedirectAllowed("https://b/cb") {
		t.Error("accepted")
	}
}
