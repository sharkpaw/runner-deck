package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
	"github.com/sharkpaw/runner-deck/internal/githubcheck"
	"github.com/sharkpaw/runner-deck/internal/handler"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
	"github.com/sharkpaw/runner-deck/internal/secrets"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed i18n/*.json
var i18nFS embed.FS

//go:embed static/app.css static/app.js static/login.js static/logo.svg
var staticFS embed.FS

// All three are injected at build time via -ldflags "-X main.Xxx=...".
// Commit/BuildDate are left empty when not injected, and version-kit omits them from the output.
var (
	Version   = "dev"
	Commit    string
	BuildDate string
)

// assetVersion is a content fingerprint of the embedded static assets, appended as /static/app.css?v=.
// A content hash is used instead of Version: the version of a dev build is always "dev", so after editing styles, refreshing the page
// would still fetch the old cached file. When the content changes, the fingerprint changes, and the browser fetches the new one naturally.
func assetVersion() string {
	h := fnv.New64a()
	entries, err := staticFS.ReadDir("static")
	if err != nil {
		return "dev"
	}
	// ReadDir sorts by filename, so identical content always yields the same fingerprint
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			continue
		}
		_, _ = h.Write([]byte(e.Name()))
		_, _ = h.Write(b)
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

// staticHandler serves the embedded /static assets.
// Requests with ?v= (the kind the page builds itself) get long caching: the URL already carries the content fingerprint, so when the content changes the URL changes too;
// a bare address without v may only use conditional caching, so a manual request can't end up holding a copy that never refreshes.
func staticHandler() echo.HandlerFunc {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	srv := http.StripPrefix("/static", http.FileServer(http.FS(sub)))
	return func(c echo.Context) error {
		if c.QueryParam("v") != "" {
			c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			c.Response().Header().Set("Cache-Control", "no-cache")
		}
		srv.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}

type templateRenderer struct {
	templates *template.Template
}

func (t *templateRenderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	// ParseFS may name templates by basename or by full path depending on the Go version, so try both
	for _, templateName := range []string{name, "templates/" + name} {
		if t.templates.Lookup(templateName) != nil {
			return t.templates.ExecuteTemplate(w, templateName, data)
		}
	}
	return t.templates.ExecuteTemplate(w, name, data) // One last attempt, so an explicit error is returned
}

func newTemplateRenderer() *templateRenderer {
	tpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))
	return &templateRenderer{templates: tpl}
}

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	handler.Version = Version
	handler.AssetVersion = assetVersion()
	handler.Commit = Commit
	handler.BuildDate = BuildDate

	// -version prints the full build info (including commit and build time). It runs locally and is not exposed externally;
	// the HTTP /version endpoint still only returns the version number, see the note on handler.VersionInfo.
	if *showVersion {
		fmt.Println(handler.BuildInfo().Full())
		os.Exit(0)
	}

	setupLogging()

	// Before anything opens files: the config load may create files, and they must belong to the app user.
	if err := dropPrivileges(systemPrivilegeOps, dockerSock, managerDirs(*configPath)...); err != nil {
		log.Fatalf("cannot drop root privileges: %v", err)
	}

	handler.ConfigPath = *configPath
	handler.Events = eventlog.ForConfig(*configPath)
	handler.Metrics = eventlog.MetricsForConfig(*configPath)
	store := secrets.NewStore(*configPath)
	handler.Secrets = store
	handler.StartRegistrationWorker()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("cannot load the configuration: %v", err)
	}
	if cfg.Runners.ContainerMode && runner.ManagerDockerHostIsDind() {
		log.Printf("warning: container mode is on, but DOCKER_HOST points at TCP (DinD). The Manager needs the host Docker socket to create, start and stop runner containers. Remove or comment out DOCKER_HOST=tcp://runner-dind:2375 in .env")
	}
	// Startup self-check: surface configuration problems here, rather than finding out when the first Job fails.
	// Only read-only checks are performed, and no failure blocks startup (so existing deployments are not disrupted).
	preflightCtx, preflightCancel := context.WithTimeout(context.Background(), 20*time.Second)
	runner.LogPreflight(runner.Preflight(preflightCtx, cfg))
	preflightCancel()

	e := newEchoServer()

	addr := listenAddr(cfg)
	srv := newHTTPServer(addr, e)
	// Move any PAT that may have been left in the runner directory to the credentials directory, and delete it from the runner directory.
	// This must happen before the runner is launched: in container mode that directory is mounted into the runner container in its entirety,
	// so as soon as the container starts, a Job could read a token that, for an organization target, is effectively admin:org.
	secrets.MigrateAll(cfg, store)
	go runAutoStartRunners(*configPath)
	go runRegistrationCheck(*configPath, store)
	handler.Events.Append(eventlog.Event{Level: eventlog.LevelInfo, Code: "manager_started", Detail: Version})
	go handler.NewWatcher(func() (*config.Config, error) { return config.Load(*configPath) }).Run(context.Background(), 20*time.Second)
	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		// This used to be log.Fatal: exiting right away without shutting the HTTP server down cleanly meant the step below that stops the runners
		// never ran, and running Jobs were still SIGKILLed. A failed clean shutdown only affects a few in-flight connections,
		// and should not also abandon the runners' graceful stop.
		log.Printf("shutting down the HTTP server failed: %v", err)
	}
	// Re-read the config: the UI may have changed it while the process has been running (including container_mode).
	// If it can't be read, proceed with the copy we already have; that is still better than stopping nothing.
	if reloaded, err := config.Load(*configPath); err == nil {
		cfg = reloaded
	} else {
		log.Printf("cannot reload the configuration while shutting down, using the one loaded at startup: %v", err)
	}
	// In container mode the runner container has its own lifecycle, independent of the Manager: restarting or upgrading the Manager
	// should not stop other people's Jobs as a side effect. In default mode it is the opposite: the runner processes live inside this container,
	// so once the Manager goes away, the kernel will SIGKILL them.
	if !cfg.Runners.ContainerMode {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), runnerproc.StopGracePeriod)
		stopDefaultModeRunners(stopCtx, cfg, runnerproc.StopGracePeriod-runnerproc.ShutdownReserve)
		stopCancel()
	}
	log.Println("stopped")
}

// newHTTPServer builds the Manager's HTTP server.
//
// An http.Server without timeouts can wait indefinitely on a peer that has stopped talking, and a single half-open connection can hold
// a goroutine and a file descriptor until the process exits. The Manager sits on the internal network, often behind a reverse proxy, so a safety net is worthwhile.
//
// **WriteTimeout is deliberately left unset**: it counts from when the response headers begin to be written, but the handler has to finish running before any response is written.
// RecreateRunner's lifecycleContext can last up to 90 seconds (stop the container + remove + create + start + wait for the Agent to be ready),
// so any WriteTimeout shorter than that would cut the connection after the operation has **already succeeded**: the container is rebuilt,
// but the UI shows a network error, and the user clicks again. ReadTimeout and IdleTimeout already guard against
// slow reads and idle connections, and the write side is left to lifecycleContext's own timeout.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// stopDefaultModeRunners stops the runners running inside this process in default mode, and waits for them to exit.
//
// In default mode run.sh is a child process of the Manager, in the same PID namespace. When the Manager exits,
// the kernel SIGKILLs every process remaining in the namespace: Jobs are not canceled gracefully, and GitHub has to wait
// for the "lost communication" timeout before marking them failed. docker compose restart or upgrading the image both take this path.
//
// If the runners have not exited by the time the grace period ends, return anyway: waiting longer would let docker stop's timer
// SIGKILL the Manager itself before it can even log a "still not stopped" message.
func stopDefaultModeRunners(ctx context.Context, cfg *config.Config, grace time.Duration) {
	var running []runner.RunnerInfo
	for _, info := range runner.List(cfg) {
		if info.Running {
			running = append(running, info)
		}
	}
	if len(running) == 0 {
		return
	}
	log.Printf("stopping %d runner(s) before exiting (up to %s)", len(running), grace)
	var pending []runner.RunnerInfo
	for _, info := range running {
		// runner.Stop sends SIGTERM to the watchdog script before the listener; this order must be kept:
		// Stopping the listener first would make run.sh's while loop immediately start a new one.
		if err := runner.Stop(info.InstallDir); err != nil {
			log.Printf("stopping runner %s failed: %v", info.Name, err)
			continue
		}
		pending = append(pending, info)
	}
	if len(pending) == 0 {
		return
	}
	deadline := time.Now().Add(grace)
	for {
		left := pending[:0]
		for _, info := range pending {
			if runnerproc.Running(info.InstallDir) {
				left = append(left, info)
			}
		}
		pending = left
		if len(pending) == 0 {
			log.Print("runners stopped")
			return
		}
		if !time.Now().Before(deadline) {
			names := make([]string, 0, len(pending))
			for _, info := range pending {
				names = append(names, info.Name)
			}
			log.Printf("runner(s) still running after %s, exiting anyway: %s", grace, strings.Join(names, ", "))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(runnerPollInterval):
		}
	}
}

// runnerPollInterval is the polling interval while waiting for the Runner to exit.
// Each poll scans procfs, and 200ms is fast enough (relative to the grace period of tens of seconds)
// without competing with the Job for CPU in a container that is busy shutting down.
const runnerPollInterval = 200 * time.Millisecond

// runAutoStartRunners runs once, delayed after startup: it starts every registered runner that is not running (so DinD/manager restarts recover)
func runAutoStartRunners(configPath string) {
	const delay = 15 * time.Second
	time.Sleep(delay)
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("cannot load the configuration while auto-starting runners: %v", err)
		return
	}
	startIdleRunners(context.Background(), cfg, "auto-start")
}

// startIdleRunners starts registered runners that are not running; used by both the one-time startup launch and the periodic check.
//
// The check uses ListWithLiveStatus rather than List: in container mode the Manager and Runner are not in the same
// PID namespace, so List's Running is always false, and starting based on it would relaunch every runner each round.
// Runners whose status cannot be detected (StatusUnknown) are always skipped: if we are not sure it is not running, leave it alone.
func startIdleRunners(ctx context.Context, cfg *config.Config, action string) {
	for _, info := range runner.ListWithLiveStatus(ctx, cfg) {
		if info.Status != runner.StatusInstalled || info.Running {
			continue
		}
		if err := runner.StartIfInstalled(ctx, cfg, info.Name, info.InstallDir); err != nil {
			log.Printf("%s of runner %s failed: %v", action, info.Name, err)
		} else {
			log.Printf("%s of runner %s done", action, info.Name)
		}
	}
}

// runRegistrationCheck runs every 5 minutes: it loads the config, checks whether each runner shows up on GitHub, and writes .github_status.json for the UI.
func runRegistrationCheck(configPath string, store *secrets.Store) {
	const interval = 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Wait a little after startup before the first run, to avoid racing with HTTP startup.
	time.Sleep(30 * time.Second)
	firstRun := true
	for {
		cfg, err := config.Load(configPath)
		if err == nil {
			githubcheck.Run(cfg, store)
			// Do not start runners on the first run, to avoid overlapping with runAutoStartRunners (15s) and starting the same runner twice.
			if !firstRun {
				startIdleRunners(context.Background(), cfg, "periodic start")
			}
			firstRun = false
		}
		<-ticker.C
	}
}

// httpErrorHandler renders all errors as {"message": ...}; echo.HTTPError keeps its own status code.
func httpErrorHandler(err error, c echo.Context) {
	code := http.StatusInternalServerError
	msg := err.Error()
	if he, ok := err.(*echo.HTTPError); ok {
		code = he.Code
		if m, ok := he.Message.(string); ok {
			msg = m
		}
	}
	_ = c.JSON(code, map[string]string{"message": msg})
}

// safeMethods are methods that do not change state; CSRF does not apply to them.
var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// trustedOrigins reads TRUSTED_ORIGINS (comma-separated), allowing extra origins through csrfGuardMiddleware.
func trustedOrigins() []string {
	raw := strings.TrimSpace(os.Getenv("TRUSTED_ORIGINS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, strings.TrimRight(v, "/"))
		}
	}
	return out
}

// originMatchesHost reports whether Origin and the request's Host are same-origin (compares only the authority).
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == host
}

// csrfGuardMiddleware rejects write requests initiated cross-site.
//
// Write endpoints used to accept both JSON and form encoding, and a cross-site form submission is a CORS "simple request":
// it skips preflight, the browser sends it anyway, and it attaches the origin's cached Basic Auth credentials. So merely getting an admin
// to open a malicious page could add, start, or stop runners. PUT and DELETE always trigger preflight and never reach this point;
// the real exposure is POST: /api/runners and /api/runners/:name/{start,stop,recreate}.
//
// Sec-Fetch-Site is checked first: the browser computes it locally, so reverse proxies that rewrite Host do not affect it
// (Chrome 76+, Firefox 90+, Safari 16.4+). If it is missing, fall back to comparing Origin with Host;
// all modern browsers send Origin on cross-site POST, so this layer is sufficient.
//
// Requests with neither header were not sent by a browser (curl, CI scripts). They do not carry the user's cookies or
// cached Basic Auth, so they are not CSRF and are allowed; otherwise this API could not be scripted.
// If a reverse proxy rewrites Host and causes false rejections, allow the origin explicitly via TRUSTED_ORIGINS.
func csrfGuardMiddleware(trusted []string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			if safeMethods[req.Method] {
				return next(c)
			}
			origin := strings.TrimRight(strings.TrimSpace(req.Header.Get("Origin")), "/")
			if origin != "" {
				for _, t := range trusted {
					if strings.EqualFold(origin, t) {
						return next(c)
					}
				}
			}
			// The browser-reported initiator is the most reliable signal
			if site := strings.TrimSpace(req.Header.Get("Sec-Fetch-Site")); site != "" {
				if site == "same-origin" {
					return next(c)
				}
				return echo.NewHTTPError(http.StatusForbidden,
					handler.Trf(c, "api.csrf_cross_site", site))
			}
			// Older browsers do not send Sec-Fetch-Site, but cross-site POST always carries Origin
			if origin != "" {
				if originMatchesHost(origin, req.Host) {
					return next(c)
				}
				return echo.NewHTTPError(http.StatusForbidden,
					handler.Trf(c, "api.csrf_origin_mismatch", origin, req.Host))
			}
			// Non-browser requests do not automatically carry credentials, so they are allowed
			return next(c)
		}
	}
}

// loadI18n reads the embedded language files
func loadI18n(lang string) (map[string]string, error) {
	data, err := i18nFS.ReadFile("i18n/" + lang + ".json")
	if err != nil {
		return nil, err
	}
	var t map[string]string
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return t, nil
}

// newEchoServer assembles the whole HTTP service: middleware order, error format, templates, i18n, and the route table.
//
// It is split out of main so the whole thing can be tested. Testing csrfGuardMiddleware alone only proves that the function
// judges correctly, but cannot prove it is actually attached in front of the write endpoints. "Middleware written but never attached" is exactly the kind of
// protection's most typical failure mode. v1.6.0 split out auth, the route table, and the listen address for the same reason. version-check-ignore
func newEchoServer() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover(), requestLogger(), middleware.Secure())
	// Every write endpoint takes only a small JSON body (a runner name, target, labels). Without a limit,
	// a request body that keeps streaming would be read into memory in full, and Basic Auth is only checked after that.
	e.Use(middleware.BodyLimit("1M"))
	e.Use(metricsMiddleware())
	e.HTTPErrorHandler = httpErrorHandler
	// Place this before auth. Cross-site requests should end here regardless of whether they carry credentials
	trusted := trustedOrigins()
	e.Use(csrfGuardMiddleware(trusted))
	if len(trusted) > 0 {
		log.Printf("CSRF trusted origins: %s", strings.Join(trusted, ", "))
	}
	if mw, user := basicAuthMiddleware(); mw != nil {
		e.Use(mw)
		log.Printf("Login enabled (user: %s)", user)
	}
	e.Renderer = newTemplateRenderer()
	// Swapping the loader must also clear the cache: the bundle caches "not found" results too (when the loader is nil,
	// it caches an empty table), so installing a loader without clearing it leaves the earlier empty table blocking everything. In production
	// it is installed only once, but this invariant should not rely on "installed only once" holding; in tests a request runs first and the loader is installed afterward,
	// so the assertion received the key "api.csrf_cross_site" and happily accepted that key name.
	handler.I18nLoader = loadI18n
	handler.ResetI18nCache()
	registerRoutes(e)
	return e
}

// registerRoutes mounts all routes
func registerRoutes(e *echo.Echo) {
	e.GET("/health", handler.Health)
	e.GET("/ready", handler.Ready)
	e.GET("/version", handler.VersionInfo)
	// It is not in the Skipper, so once Basic Auth is configured, /metrics also requires credentials
	e.GET(metricsPath, metricsHandler())
	e.GET("/", handler.Index)
	e.GET("/login", loginPage)
	e.POST("/login", loginSubmit)
	e.POST("/logout", logoutSubmit)
	// Register HEAD too: if only GET is mounted, health-check scripts or proxies sending HEAD to static assets get a 405
	staticH := staticHandler()
	e.GET("/static/*", staticH)
	e.HEAD("/static/*", staticH)
	e.GET("/api/system", handler.SystemInfo)
	e.PUT("/api/settings/resources", handler.UpdateResourceLimits)
	e.POST("/api/docker/clean", handler.CleanDocker)
	e.POST("/api/runners/:name/clean", handler.CleanRunnerWork)
	e.GET("/api/events", handler.ListEvents)
	e.GET("/api/stats", handler.JobStatistics)
	e.GET("/api/metrics", handler.MetricHistory)
	e.GET("/api/runners/:name/logs", handler.RunnerLogs)
	e.GET("/api/runners", handler.ListRunners)
	e.GET("/api/runner-rows", handler.RunnerRows)
	e.GET("/api/runners/:name", handler.GetRunner)
	e.POST("/api/runners", handler.AddRunner)
	// Static paths, placed outside /api/runners/:name so they do not compete with a runner named check for the route
	e.GET("/api/runner-precheck", handler.PrecheckRunner)
	e.PUT("/api/runners/:name", handler.UpdateRunner)
	e.DELETE("/api/runners/:name", handler.RemoveRunnerByName)
	e.POST("/api/runners/:name/start", handler.StartRunner)
	e.POST("/api/runners/:name/stop", handler.StopRunner)
	e.POST("/api/runners/:name/recreate", handler.RecreateRunner)
}

// listenAddr derives the listen address from the config; when no port is set it falls back to :8080
func listenAddr(cfg *config.Config) string {
	if cfg == nil || cfg.Server.Port <= 0 {
		return ":8080"
	}
	if cfg.Server.Addr != "" {
		return fmt.Sprintf("%s:%d", cfg.Server.Addr, cfg.Server.Port)
	}
	return fmt.Sprintf(":%d", cfg.Server.Port)
}
