package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/auth"
	"github.com/sharkpaw/runner-deck/internal/handler"
	"github.com/sharkpaw/runner-deck/internal/mcp"
)

const (
	authFileName = "auth.json"
	sessionIDKey = "session_id"
)

// cookieRenewal is how old a cookie gets before it is reissued, which also extends its session.
var cookieRenewal = 24 * time.Hour

// authStore holds sessions. It is set by newEchoServer.
var authStore *auth.Store

// registerLimiter keeps unauthenticated client registration from growing the file without limit.
var registerLimiter = &loginThrottle{}

type internalKey struct{}

// isInternal reports a request the MCP server made to the API on behalf of a verified token. It is
// a context value, so nothing arriving over the network can claim it.
func isInternal(r *http.Request) bool { return r.Context().Value(internalKey{}) != nil }

func authEnabled() bool {
	_, pw := credentials()
	return pw != ""
}

func openAuthStore() *auth.Store {
	user, pw := credentials()
	path := ""
	if handler.ConfigPath != "" {
		path = filepath.Join(filepath.Dir(handler.ConfigPath), authFileName)
	}
	store, err := auth.Open(path, user, pw)
	if err != nil {
		log.Fatalf("cannot open the session store: %v", err)
	}
	return store
}

func requestMeta(c echo.Context) auth.Meta {
	return auth.Meta{UserAgent: c.Request().UserAgent(), IP: c.RealIP()}
}

// baseURL is the address clients reach this Manager at: PUBLIC_URL, or else what the proxy reports.
func baseURL(c echo.Context) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/"); v != "" {
		return v
	}
	scheme := "http"
	if c.IsTLS() || c.Request().Header.Get(echo.HeaderXForwardedProto) == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request().Host
}

// tokenEndpoints take no cookies, only a body or a bearer token, so CSRF does not apply and any
// origin may call them.
func isTokenEndpoint(path string) bool {
	switch path {
	case "/mcp", "/oauth/token", "/oauth/register", "/oauth/revoke", "/oauth/ticket":
		return true
	}
	return strings.HasPrefix(path, "/.well-known/")
}

func allowAnyOrigin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Expose-Headers", "WWW-Authenticate")
		if c.Request().Method == http.MethodOptions {
			return c.NoContent(http.StatusNoContent)
		}
		return next(c)
	}
}

func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get(echo.HeaderAuthorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func unauthorizedBearer(c echo.Context) error {
	c.Response().Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, baseURL(c)))
	return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
}

func insufficientScope(c echo.Context) error {
	c.Response().Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope="%s"`, auth.ScopeWrite))
	return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient_scope"})
}

// bearerAllows decides what a token may do on the API. Reading the sessions list shows where the
// owner signs in from, so it takes full access too.
func bearerAllows(v auth.Verified, r *http.Request) bool {
	if v.CanWrite() {
		return true
	}
	return safeMethods[r.Method] && !strings.HasPrefix(r.URL.Path, "/api/sessions")
}

// cookieSession returns the session behind a valid cookie, renewing an old cookie along the way.
func cookieSession(c echo.Context) (string, bool) {
	ck, err := c.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	v, err := authStore.VerifyCookie(ck.Value)
	if err != nil {
		return "", false
	}
	if time.Since(v.IssuedAt) >= cookieRenewal {
		if value, err := authStore.RenewCookie(v.SessionID); err == nil {
			setSessionCookie(c, value, int(auth.SessionTTL/time.Second))
		}
	}
	return v.SessionID, true
}

func metadataNotAuthEnabled(c echo.Context) error {
	return echo.NewHTTPError(http.StatusNotFound)
}

func authServerMetadata(c echo.Context) error {
	if !authEnabled() {
		return metadataNotAuthEnabled(c)
	}
	base := baseURL(c)
	return c.JSON(http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"revocation_endpoint":                   base + "/oauth/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{auth.ScopeRead, auth.ScopeWrite},
	})
}

func protectedResourceMetadata(c echo.Context) error {
	if !authEnabled() {
		return metadataNotAuthEnabled(c)
	}
	base := baseURL(c)
	return c.JSON(http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{auth.ScopeRead, auth.ScopeWrite},
		"bearer_methods_supported": []string{"header"},
	})
}

func oauthRegister(c echo.Context) error {
	if !authEnabled() {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	now := time.Now()
	ip := c.RealIP()
	if registerLimiter.blocked(ip, now) {
		return c.JSON(http.StatusTooManyRequests, map[string]string{"error": "too_many_requests"})
	}
	registerLimiter.fail(ip, now)
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
	}
	client, err := authStore.Register(req.ClientName, req.RedirectURIs)
	if errors.Is(err, auth.ErrBadRedirect) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// authorizeRequest is what /oauth/authorize receives, by query or by the consent form.
type authorizeRequest struct {
	ClientID, RedirectURI, ResponseType, Challenge, Method, State, Scope string
}

func readAuthorize(c echo.Context) authorizeRequest {
	return authorizeRequest{
		ClientID:     c.FormValue("client_id"),
		RedirectURI:  c.FormValue("redirect_uri"),
		ResponseType: c.FormValue("response_type"),
		Challenge:    c.FormValue("code_challenge"),
		Method:       c.FormValue("code_challenge_method"),
		State:        c.FormValue("state"),
		Scope:        c.FormValue("scope"),
	}
}

func (a authorizeRequest) redirect(c echo.Context, params url.Values) error {
	target, err := url.Parse(a.RedirectURI)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	q := target.Query()
	for k, v := range params {
		q[k] = v
	}
	if a.State != "" {
		q.Set("state", a.State)
	}
	q.Set("iss", baseURL(c))
	target.RawQuery = q.Encode()
	return c.Redirect(http.StatusSeeOther, target.String())
}

// checkAuthorize returns the client. Before the redirect address is trusted, errors must not go to it.
func checkAuthorize(c echo.Context, a authorizeRequest) (auth.Client, error) {
	if !authEnabled() {
		return auth.Client{}, echo.NewHTTPError(http.StatusNotFound)
	}
	client, ok := authStore.FindClient(a.ClientID)
	if !ok || !client.RedirectAllowed(a.RedirectURI) {
		return auth.Client{}, echo.NewHTTPError(http.StatusBadRequest, handler.Trf(c, "consent.invalid"))
	}
	return client, nil
}

func oauthAuthorizePage(c echo.Context) error {
	a := readAuthorize(c)
	client, err := checkAuthorize(c, a)
	if err != nil {
		return err
	}
	if a.ResponseType != "code" {
		return a.redirect(c, url.Values{"error": {"unsupported_response_type"}})
	}
	if a.Method != "S256" || len(a.Challenge) < 43 {
		return a.redirect(c, url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is required"}})
	}
	if _, ok := cookieSession(c); !ok {
		return c.Redirect(http.StatusSeeOther, "/login?next="+url.QueryEscape(c.Request().URL.RequestURI()))
	}
	data := handler.PageData(c)
	data["ClientName"] = client.Name
	data["RedirectHost"] = redirectLabel(a.RedirectURI)
	data["Request"] = a
	// The desktop app shows the whole panel, so it has no use for less. A client asking only to read
	// has nothing to choose. Anything else starts on the narrower choice.
	data["FullAccessOnly"] = a.ClientID == auth.DesktopClientID
	data["CanChoose"] = a.ClientID != auth.DesktopClientID && auth.RequestedScope(a.Scope) == auth.ScopeWrite
	return c.Render(http.StatusOK, "consent.html", data)
}

// redirectLabel names where the code will go, which is what tells a look-alike client apart.
func redirectLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host != "" {
		return u.Host
	}
	return u.Scheme + ":"
}

func oauthAuthorizeSubmit(c echo.Context) error {
	a := readAuthorize(c)
	if _, err := checkAuthorize(c, a); err != nil {
		return err
	}
	if _, ok := cookieSession(c); !ok {
		return echo.NewHTTPError(http.StatusUnauthorized, handler.Trf(c, "consent.invalid"))
	}
	if c.FormValue("decision") != "approve" {
		return a.redirect(c, url.Values{"error": {"access_denied"}})
	}
	scope := auth.GrantedScope(auth.RequestedScope(a.Scope), c.FormValue("access"))
	if a.ClientID == auth.DesktopClientID {
		scope = auth.ScopeWrite
	}
	code, err := authStore.NewCode(a.ClientID, a.RedirectURI, a.Challenge, scope)
	if err != nil {
		return a.redirect(c, url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is required"}})
	}
	return a.redirect(c, url.Values{"code": {code}})
}

func oauthError(c echo.Context, code string) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusBadRequest, map[string]string{"error": code})
}

func oauthToken(c echo.Context) error {
	if !authEnabled() {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	var (
		tokens auth.Tokens
		err    error
	)
	switch c.FormValue("grant_type") {
	case "authorization_code":
		tokens, err = authStore.ExchangeCode(c.FormValue("code"), c.FormValue("code_verifier"),
			c.FormValue("client_id"), c.FormValue("redirect_uri"), requestMeta(c))
	case "refresh_token":
		tokens, err = authStore.Refresh(c.FormValue("refresh_token"), c.FormValue("client_id"), requestMeta(c))
	default:
		return oauthError(c, "unsupported_grant_type")
	}
	switch {
	case errors.Is(err, auth.ErrInvalidClient):
		return oauthError(c, "invalid_client")
	case err != nil:
		return oauthError(c, "invalid_grant")
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]any{
		"access_token":  tokens.Access,
		"token_type":    "Bearer",
		"expires_in":    tokens.ExpiresIn,
		"refresh_token": tokens.Refresh,
		"scope":         tokens.Scope,
	})
}

func oauthRevoke(c echo.Context) error {
	if !authEnabled() {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := authStore.RevokeRefresh(c.FormValue("token")); err != nil {
		return err
	}
	return c.NoContent(http.StatusOK)
}

// oauthTicket lets an app holding tokens open the panel in its own window, signed in.
func oauthTicket(c echo.Context) error {
	if !authEnabled() {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	v, err := authStore.VerifyAccess(bearerToken(c.Request()))
	if err != nil {
		return unauthorizedBearer(c)
	}
	// A ticket opens the whole panel, so a read-only token must not get one.
	if !v.CanWrite() {
		return insufficientScope(c)
	}
	ticket, _ := authStore.NewTicket(v.SessionID)
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]string{"url": baseURL(c) + "/login/ticket?t=" + url.QueryEscape(ticket)})
}

func loginTicket(c echo.Context) error {
	if !authEnabled() {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	value, err := authStore.RedeemTicket(c.QueryParam("t"))
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/login")
	}
	setSessionCookie(c, value, int(auth.SessionTTL/time.Second))
	return c.Redirect(http.StatusSeeOther, "/")
}

// sessionView is one row of the sessions page.
type sessionView struct {
	auth.Session
	Current bool `json:"current"`
}

func listSessions(c echo.Context) error {
	current, _ := c.Get(sessionIDKey).(string)
	sessions := authStore.Sessions()
	out := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionView{Session: s, Current: s.ID == current})
	}
	return c.JSON(http.StatusOK, map[string]any{"sessions": out, "mcp_url": baseURL(c) + "/mcp"})
}

func revokeSession(c echo.Context) error {
	found, err := authStore.Revoke(c.Param("id"))
	if err != nil {
		return err
	}
	if !found {
		return echo.NewHTTPError(http.StatusNotFound, handler.Trf(c, "api.session_not_found"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": handler.Trf(c, "api.session_ended")})
}

// bufferWriter collects a response from the API for an MCP tool.
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferWriter) Header() http.Header { return w.header }
func (w *bufferWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}
func (w *bufferWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func apiCaller(e *echo.Echo) mcp.Caller {
	return func(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
		req, err := http.NewRequestWithContext(context.WithValue(ctx, internalKey{}, true), method, path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		if len(body) > 0 {
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		}
		w := &bufferWriter{header: http.Header{}}
		e.ServeHTTP(w, req)
		return w.status, w.body.Bytes(), nil
	}
}

func mcpHandler(e *echo.Echo) echo.HandlerFunc {
	server := &mcp.Server{Name: "runner-deck", Version: handler.Version, Call: apiCaller(e)}
	return func(c echo.Context) error {
		canWrite := true
		if authEnabled() {
			v, err := authStore.VerifyAccess(bearerToken(c.Request()))
			if err != nil {
				return unauthorizedBearer(c)
			}
			canWrite = v.CanWrite()
		}
		if c.Request().Method != http.MethodPost {
			c.Response().Header().Set("Allow", http.MethodPost)
			return c.NoContent(http.StatusMethodNotAllowed)
		}
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest)
		}
		reply := server.Handle(c.Request().Context(), body, canWrite)
		if reply == nil {
			return c.NoContent(http.StatusAccepted)
		}
		return c.JSON(http.StatusOK, reply)
	}
}

func registerAuthRoutes(e *echo.Echo) {
	open := func(method, path string, h echo.HandlerFunc) {
		e.Add(method, path, h, allowAnyOrigin)
		e.Add(http.MethodOptions, path, h, allowAnyOrigin)
	}
	open(http.MethodGet, "/.well-known/oauth-authorization-server", authServerMetadata)
	open(http.MethodGet, "/.well-known/oauth-protected-resource", protectedResourceMetadata)
	open(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", protectedResourceMetadata)
	open(http.MethodPost, "/oauth/register", oauthRegister)
	open(http.MethodPost, "/oauth/token", oauthToken)
	open(http.MethodPost, "/oauth/revoke", oauthRevoke)
	open(http.MethodPost, "/oauth/ticket", oauthTicket)
	mcpRoute := allowAnyOrigin(mcpHandler(e))
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
		e.Add(method, "/mcp", mcpRoute)
	}
	e.GET("/oauth/authorize", oauthAuthorizePage)
	e.POST("/oauth/authorize", oauthAuthorizeSubmit)
	e.GET("/login/ticket", loginTicket)
	e.GET("/api/sessions", listSessions)
	e.DELETE("/api/sessions/:id", revokeSession)
}

// safeNext keeps the sign-in page from redirecting anywhere but back to an authorization request.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/oauth/authorize?") {
		return next
	}
	return "/"
}
