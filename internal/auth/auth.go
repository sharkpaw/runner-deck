// Package auth holds the Manager's sessions and its OAuth 2.1 authorization server state.
//
// There is one account, the configured login. Every sign-in is a session in a small file, so it can
// be listed and ended. Access tokens are short JWTs tied to a session; the refresh token behind them
// rotates on each use and keeps the session alive for SessionTTL after the last refresh.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/sharkpaw/runner-deck/internal/atomicfile"
)

const (
	// SessionTTL is how long a session lasts after it was last refreshed.
	SessionTTL = 180 * 24 * time.Hour
	AccessTTL  = 15 * time.Minute

	codeTTL   = time.Minute
	ticketTTL = time.Minute
	// A client that lost the response to a refresh may repeat it this soon. Later, a reused
	// refresh token means it was copied, and the session ends.
	refreshGrace = time.Minute
	// Writing last-used times is not worth a disk write per request.
	touchEvery = 5 * time.Minute
	maxClients = 200
	audience   = "runner-deck"

	KindBrowser = "browser"
	KindOAuth   = "oauth"

	// DesktopClientID is the desktop app, which needs no registration.
	DesktopClientID = "runner-deck-desktop"
)

var (
	ErrInvalidToken  = errors.New("invalid token")
	ErrInvalidGrant  = errors.New("invalid grant")
	ErrInvalidClient = errors.New("invalid client")
	ErrBadRedirect   = errors.New("invalid redirect uri")
)

// Session is one sign-in.
type Session struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ClientID        string    `json:"client_id,omitempty"`
	ClientName      string    `json:"client_name,omitempty"`
	UserAgent       string    `json:"user_agent,omitempty"`
	IP              string    `json:"ip,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	LastUsedAt      time.Time `json:"last_used_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	RefreshHash     string    `json:"refresh_hash,omitempty"`
	PrevRefreshHash string    `json:"prev_refresh_hash,omitempty"`
	RotatedAt       time.Time `json:"rotated_at,omitempty"`
}

// Client is an OAuth client.
type Client struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

// Tokens is what a token request returns.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresIn int
	SessionID string
}

// Meta describes where a sign-in came from.
type Meta struct {
	UserAgent string
	IP        string
}

type grant struct {
	clientID  string
	redirect  string
	challenge string
	expires   time.Time
}

type ticket struct {
	sessionID string
	expires   time.Time
}

type fileData struct {
	Secret   string     `json:"secret"`
	Sessions []*Session `json:"sessions"`
	Clients  []*Client  `json:"clients"`
}

// Store keeps sessions in a file. An empty path keeps them in memory, for tests.
type Store struct {
	path string
	user string
	key  []byte
	now  func() time.Time

	mu      sync.Mutex
	data    fileData
	codes   map[string]grant
	tickets map[string]ticket
}

// Open loads the store. The signing key mixes in the password, so changing it ends every session.
func Open(path, user, password string) (*Store, error) {
	s := &Store{path: path, user: user, now: time.Now, codes: map[string]grant{}, tickets: map[string]ticket{}}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, &s.data); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	if s.data.Secret == "" {
		s.data.Secret = randomToken(32)
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256([]byte("runner-deck/auth/v1\x00" + s.data.Secret + "\x00" + password))
	s.key = sum[:]
	return s, nil
}

// randomToken cannot fail: crypto/rand.Read never returns an error, it crashes the program instead.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(s.path, raw, 0o600)
}

func (s *Store) refreshHash(token string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) find(id string) *Session {
	for _, sess := range s.data.Sessions {
		if sess.ID == id {
			return sess
		}
	}
	return nil
}

func (s *Store) dropExpired(now time.Time) {
	kept := s.data.Sessions[:0]
	for _, sess := range s.data.Sessions {
		if now.Before(sess.ExpiresAt) {
			kept = append(kept, sess)
		}
	}
	s.data.Sessions = kept
}

func (s *Store) newSession(kind, clientID, clientName string, meta Meta, now time.Time) *Session {
	s.dropExpired(now)
	sess := &Session{
		ID: randomToken(16), Kind: kind, ClientID: clientID, ClientName: clientName,
		UserAgent: truncate(meta.UserAgent, 300), IP: meta.IP,
		CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(SessionTTL),
	}
	s.data.Sessions = append(s.data.Sessions, sess)
	return sess
}

func truncate(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}

func (s *Store) sign(sessionID string, kind string, ttl time.Duration, now time.Time) string {
	// HS256 with a byte key has no way to fail.
	signed, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": s.user, "aud": audience, "sid": sessionID, "typ": kind, "jti": randomToken(12),
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
	}).SignedString(s.key)
	return signed
}

// StartBrowserSession records a password sign-in and returns its cookie value.
func (s *Store) StartBrowserSession(meta Meta) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess := s.newSession(KindBrowser, "", "", meta, now)
	if err := s.save(); err != nil {
		return "", err
	}
	return s.sign(sess.ID, "cookie", SessionTTL, now), nil
}

// Verified is a token that passed verification.
type Verified struct {
	SessionID string
	IssuedAt  time.Time
}

func (s *Store) verify(token, typ string) (Verified, error) {
	claims := jwt.MapClaims{}
	now := s.now()
	parsed, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(audience), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !parsed.Valid {
		return Verified{}, ErrInvalidToken
	}
	sid, _ := claims["sid"].(string)
	if got, _ := claims["typ"].(string); got != typ || sid == "" {
		return Verified{}, ErrInvalidToken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.find(sid)
	// The session is what makes a signed token revocable.
	if sess == nil || !now.Before(sess.ExpiresAt) {
		return Verified{}, ErrInvalidToken
	}
	if now.Sub(sess.LastUsedAt) >= touchEvery {
		sess.LastUsedAt = now
		_ = s.save()
	}
	issued, _ := claims.GetIssuedAt()
	v := Verified{SessionID: sid}
	if issued != nil {
		v.IssuedAt = issued.Time
	}
	return v, nil
}

// VerifyCookie checks a browser cookie.
func (s *Store) VerifyCookie(token string) (Verified, error) { return s.verify(token, "cookie") }

// VerifyAccess checks a bearer access token.
func (s *Store) VerifyAccess(token string) (Verified, error) { return s.verify(token, "access") }

// RenewCookie issues a fresh cookie and extends the session.
func (s *Store) RenewCookie(sessionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.find(sessionID)
	if sess == nil {
		return "", ErrInvalidToken
	}
	now := s.now()
	sess.ExpiresAt = now.Add(SessionTTL)
	sess.LastUsedAt = now
	if err := s.save(); err != nil {
		return "", err
	}
	return s.sign(sessionID, "cookie", SessionTTL, now), nil
}

// Sessions lists live sessions, most recently used first, without their secrets.
func (s *Store) Sessions() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(s.now())
	out := make([]Session, 0, len(s.data.Sessions))
	for _, sess := range s.data.Sessions {
		c := *sess
		c.RefreshHash, c.PrevRefreshHash = "", ""
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsedAt.After(out[j].LastUsedAt) })
	return out
}

// Revoke ends a session and reports whether it existed.
func (s *Store) Revoke(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeLocked(id)
}

func (s *Store) revokeLocked(id string) (bool, error) {
	for i, sess := range s.data.Sessions {
		if sess.ID == id {
			s.data.Sessions = append(s.data.Sessions[:i], s.data.Sessions[i+1:]...)
			return true, s.save()
		}
	}
	return false, nil
}

func isLoopback(u *url.URL) bool {
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validRedirect accepts https, loopback http, and private app schemes. Never one that runs in the page.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopback(u)
	case "javascript", "data", "vbscript", "file", "about", "blob":
		return false
	default:
		return strings.Contains(u.Scheme, ".") || u.Host != "" || u.Path != ""
	}
}

// Register adds a client (RFC 7591). The oldest registrations go first once the list is full.
func (s *Store) Register(name string, redirects []string) (Client, error) {
	if len(redirects) == 0 || len(redirects) > 10 {
		return Client{}, ErrBadRedirect
	}
	for _, r := range redirects {
		if !validRedirect(r) {
			return Client{}, ErrBadRedirect
		}
	}
	name = truncate(strings.TrimSpace(name), 100)
	if name == "" {
		name = "MCP"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Client{ID: randomToken(16), Name: name, RedirectURIs: redirects, CreatedAt: s.now()}
	s.data.Clients = append(s.data.Clients, c)
	if extra := len(s.data.Clients) - maxClients; extra > 0 {
		s.data.Clients = s.data.Clients[extra:]
	}
	return *c, s.save()
}

// FindClient returns a client, including the built-in desktop app.
func (s *Store) FindClient(id string) (Client, bool) {
	if id == DesktopClientID {
		return Client{ID: id, Name: "Runner Deck Desktop", RedirectURIs: []string{"http://127.0.0.1/callback"}}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.data.Clients {
		if c.ID == id {
			return *c, true
		}
	}
	return Client{}, false
}

// RedirectAllowed matches exactly, except that a loopback address may use any port (RFC 8252).
func (c Client) RedirectAllowed(raw string) bool {
	got, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for _, allowed := range c.RedirectURIs {
		if raw == allowed {
			return true
		}
		want, err := url.Parse(allowed)
		if err == nil && want.Scheme == "http" && isLoopback(want) && got.Scheme == "http" &&
			got.Hostname() == want.Hostname() && got.Path == want.Path && got.RawQuery == want.RawQuery {
			return true
		}
	}
	return false
}

// NewCode issues a single-use authorization code. PKCE S256 is required.
func (s *Store) NewCode(clientID, redirect, challenge string) (string, error) {
	if len(challenge) < 43 {
		return "", ErrInvalidGrant
	}
	code := randomToken(32)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, g := range s.codes {
		if !now.Before(g.expires) {
			delete(s.codes, k)
		}
	}
	s.codes[code] = grant{clientID: clientID, redirect: redirect, challenge: challenge, expires: now.Add(codeTTL)}
	return code, nil
}

func (s *Store) issue(sess *Session, now time.Time) (Tokens, error) {
	refresh := randomToken(32)
	access := s.sign(sess.ID, "access", AccessTTL, now)
	sess.PrevRefreshHash = sess.RefreshHash
	sess.RotatedAt = now
	sess.RefreshHash = s.refreshHash(refresh)
	sess.LastUsedAt = now
	sess.ExpiresAt = now.Add(SessionTTL)
	if err := s.save(); err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: sess.ID + "." + refresh, ExpiresIn: int(AccessTTL / time.Second), SessionID: sess.ID}, nil
}

// ExchangeCode turns a code into tokens and a new session.
func (s *Store) ExchangeCode(code, verifier, clientID, redirect string, meta Meta) (Tokens, error) {
	client, ok := s.FindClient(clientID)
	if !ok {
		return Tokens{}, ErrInvalidClient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	g, ok := s.codes[code]
	delete(s.codes, code) // single use, even when the attempt fails
	if !ok || !now.Before(g.expires) || g.clientID != clientID || g.redirect != redirect {
		return Tokens{}, ErrInvalidGrant
	}
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(g.challenge)) != 1 {
		return Tokens{}, ErrInvalidGrant
	}
	return s.issue(s.newSession(KindOAuth, clientID, client.Name, meta, now), now)
}

// Refresh rotates a refresh token. Reuse of an old one ends the session.
func (s *Store) Refresh(token, clientID string, meta Meta) (Tokens, error) {
	sid, secret, ok := strings.Cut(token, ".")
	if !ok {
		return Tokens{}, ErrInvalidGrant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess := s.find(sid)
	if sess == nil || !now.Before(sess.ExpiresAt) || sess.Kind != KindOAuth || sess.ClientID != clientID {
		return Tokens{}, ErrInvalidGrant
	}
	hash := s.refreshHash(secret)
	current := hmac.Equal([]byte(hash), []byte(sess.RefreshHash))
	previous := sess.PrevRefreshHash != "" && hmac.Equal([]byte(hash), []byte(sess.PrevRefreshHash))
	if previous && now.Sub(sess.RotatedAt) >= refreshGrace {
		_, _ = s.revokeLocked(sid)
		return Tokens{}, ErrInvalidGrant
	}
	if !current && !previous {
		return Tokens{}, ErrInvalidGrant
	}
	if meta.IP != "" {
		sess.IP = meta.IP
	}
	return s.issue(sess, now)
}

// RevokeRefresh ends the session behind a refresh token. Unknown tokens are not an error (RFC 7009).
func (s *Store) RevokeRefresh(token string) error {
	sid, secret, ok := strings.Cut(token, ".")
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.find(sid)
	if sess == nil || !hmac.Equal([]byte(s.refreshHash(secret)), []byte(sess.RefreshHash)) {
		return nil
	}
	_, err := s.revokeLocked(sid)
	return err
}

// NewTicket issues a one-time ticket that signs a browser view in to an existing session, so a
// desktop app holding tokens can open the panel in its window.
func (s *Store) NewTicket(sessionID string) (string, error) {
	t := randomToken(32)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.tickets {
		if !now.Before(v.expires) {
			delete(s.tickets, k)
		}
	}
	s.tickets[t] = ticket{sessionID: sessionID, expires: now.Add(ticketTTL)}
	return t, nil
}

// RedeemTicket spends a ticket and returns a cookie for its session.
func (s *Store) RedeemTicket(t string) (string, error) {
	s.mu.Lock()
	v, ok := s.tickets[t]
	delete(s.tickets, t)
	now := s.now()
	s.mu.Unlock()
	if !ok || !now.Before(v.expires) {
		return "", ErrInvalidToken
	}
	return s.RenewCookie(v.sessionID)
}
