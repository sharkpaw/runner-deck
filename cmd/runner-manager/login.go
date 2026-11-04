package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sharkpaw/runner-deck/internal/handler"
	secure "github.com/soulteary/secure-kit/v2"
)

const (
	sessionCookie = "runner_deck_session"
	sessionTTL    = 7 * 24 * time.Hour

	loginWindow     = 15 * time.Minute
	loginMaxPerIP   = 10
	loginMaxOverall = 100
)

// credentials reads the configured login. An empty password means auth is off.
func credentials() (user, pass string) {
	pass = os.Getenv("BASIC_AUTH_PASSWORD")
	user = strings.TrimSpace(os.Getenv("BASIC_AUTH_USER"))
	if user == "" {
		user = "admin"
	}
	return user, pass
}

// signSession signs the expiry with a key derived from the password, so sessions need no
// storage and changing the password ends all of them.
func signSession(user, pass string, expires int64) string {
	key := sha256.Sum256([]byte("runner-deck/session/v1\x00" + pass))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(user + "\x00" + strconv.FormatInt(expires, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func newSessionValue(user, pass string, now time.Time) string {
	expires := now.Add(sessionTTL).Unix()
	return strconv.FormatInt(expires, 10) + "." + signSession(user, pass, expires)
}

func validSession(value, user, pass string, now time.Time) bool {
	expText, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(expText, 10, 64)
	if err != nil || now.Unix() >= expires {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(signSession(user, pass, expires)))
}

// loginThrottle limits wrong passwords. The per-address limit keeps one client from locking out
// everyone; the overall limit still applies when a client changes X-Forwarded-For on each try.
type loginThrottle struct {
	mu      sync.Mutex
	started time.Time
	perIP   map[string]int
	overall int
}

func (l *loginThrottle) resetIfExpired(now time.Time) {
	if l.perIP == nil || now.Sub(l.started) >= loginWindow {
		l.started = now
		l.perIP = map[string]int{}
		l.overall = 0
	}
}

func (l *loginThrottle) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resetIfExpired(now)
	return l.perIP[ip] >= loginMaxPerIP || l.overall >= loginMaxOverall
}

func (l *loginThrottle) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resetIfExpired(now)
	l.perIP[ip]++
	l.overall++
}

var loginLimiter = &loginThrottle{}

// basicAuthMiddleware protects everything except probes, the login page and static assets when
// BASIC_AUTH_PASSWORD is set; without it the middleware is nil and every endpoint is open.
//
// A valid session cookie or Basic Auth credentials both pass, so scripts and Prometheus keep
// working. Browsers opening a page go to /login instead of getting the native password dialog.
func basicAuthMiddleware() (echo.MiddlewareFunc, string) {
	expectedUser, pw := credentials()
	if pw == "" {
		return nil, ""
	}
	skip := func(c echo.Context) bool {
		p := c.Path()
		return p == "/health" || p == "/ready" || p == "/login" || p == "/static/*"
	}
	basic := middleware.BasicAuthWithConfig(middleware.BasicAuthConfig{
		Skipper: skip,
		Validator: func(username, password string, _ echo.Context) (bool, error) {
			// secure.ConstantTimeEqual pads both sides, so timing reveals neither a matching prefix
			// nor the length of the secret, which subtle.ConstantTimeCompare would.
			userOk := secure.ConstantTimeEqual(username, expectedUser)
			passOk := secure.ConstantTimeEqual(password, pw)
			return userOk && passOk, nil
		},
	})
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		withBasic := basic(next)
		return func(c echo.Context) error {
			if skip(c) {
				return next(c)
			}
			if ck, err := c.Cookie(sessionCookie); err == nil && validSession(ck.Value, expectedUser, pw, time.Now()) {
				return next(c)
			}
			if c.Request().Header.Get(echo.HeaderAuthorization) == "" {
				req := c.Request()
				if req.Method == http.MethodGet && strings.Contains(req.Header.Get(echo.HeaderAccept), "text/html") {
					return c.Redirect(http.StatusSeeOther, "/login")
				}
				// The page's own fetch calls must not trigger the browser's password dialog.
				if req.Header.Get("Sec-Fetch-Site") != "" {
					return echo.NewHTTPError(http.StatusUnauthorized, "login required")
				}
			}
			return withBasic(c)
		}
	}, expectedUser
}

func setSessionCookie(c echo.Context, value string, maxAge int) {
	req := c.Request()
	c.SetCookie(&http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.IsTLS() || req.Header.Get(echo.HeaderXForwardedProto) == "https",
	})
}

func renderLogin(c echo.Context, status int, errKey string) error {
	data := handler.PageData(c)
	data["Error"] = errKey
	return c.Render(status, "login.html", data)
}

func loginPage(c echo.Context) error {
	user, pw := credentials()
	if pw == "" {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	if ck, err := c.Cookie(sessionCookie); err == nil && validSession(ck.Value, user, pw, time.Now()) {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	return renderLogin(c, http.StatusOK, "")
}

func loginSubmit(c echo.Context) error {
	user, pw := credentials()
	if pw == "" {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	now := time.Now()
	ip := c.RealIP()
	if loginLimiter.blocked(ip, now) {
		return renderLogin(c, http.StatusTooManyRequests, "login.too_many")
	}
	userOk := secure.ConstantTimeEqual(c.FormValue("username"), user)
	passOk := secure.ConstantTimeEqual(c.FormValue("password"), pw)
	if !userOk || !passOk {
		loginLimiter.fail(ip, now)
		return renderLogin(c, http.StatusUnauthorized, "login.invalid")
	}
	setSessionCookie(c, newSessionValue(user, pw, now), int(sessionTTL/time.Second))
	return c.Redirect(http.StatusSeeOther, "/")
}

func logoutSubmit(c echo.Context) error {
	setSessionCookie(c, "", -1)
	return c.Redirect(http.StatusSeeOther, "/login")
}
