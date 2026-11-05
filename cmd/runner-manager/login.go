package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sharkpaw/runner-deck/internal/auth"
	"github.com/sharkpaw/runner-deck/internal/handler"
	secure "github.com/soulteary/secure-kit/v2"
)

const (
	sessionCookie = "runner_deck_session"

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
		if isTokenEndpoint(p) || p == "/oauth/authorize" || p == "/login/ticket" {
			return true // they check their own credentials
		}
		return p == "/health" || p == "/ready" || p == "/login" || p == "/static/*" || isInternal(c.Request())
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
			if sid, ok := cookieSession(c); ok {
				c.Set(sessionIDKey, sid)
				return next(c)
			}
			if token := bearerToken(c.Request()); token != "" {
				v, err := authStore.VerifyAccess(token)
				if err != nil {
					return unauthorizedBearer(c)
				}
				if !bearerAllows(v, c.Request()) {
					return insufficientScope(c)
				}
				c.Set(sessionIDKey, v.SessionID)
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
	if next := safeNext(c.FormValue("next")); next != "/" {
		data["Next"] = next
	}
	return c.Render(status, "login.html", data)
}

func loginPage(c echo.Context) error {
	if !authEnabled() {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	if _, ok := cookieSession(c); ok {
		return c.Redirect(http.StatusSeeOther, safeNext(c.QueryParam("next")))
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
	value, err := authStore.StartBrowserSession(requestMeta(c))
	if err != nil {
		return err
	}
	setSessionCookie(c, value, int(auth.SessionTTL/time.Second))
	return c.Redirect(http.StatusSeeOther, safeNext(c.FormValue("next")))
}

func logoutSubmit(c echo.Context) error {
	// Only a cookie's own session ends here; revoking from the sessions page covers the rest.
	if ck, err := c.Cookie(sessionCookie); err == nil {
		if v, err := authStore.VerifyCookie(ck.Value); err == nil {
			_, _ = authStore.Revoke(v.SessionID)
		}
	}
	setSessionCookie(c, "", -1)
	return c.Redirect(http.StatusSeeOther, "/login")
}
