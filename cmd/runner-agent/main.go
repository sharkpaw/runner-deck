// Runner Agent: runs inside the runner container and only controls the Runner process (start/stop) and reports health/status (/status, /health). The Manager calls it over HTTP.
// Environment variables: RUNNER_INSTALL_DIR (default /runner), AGENT_PORT (default 8081)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sharkpaw/runner-deck/internal/childenv"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
	secure "github.com/soulteary/secure-kit/v2"
)

const defaultInstallDir = "/runner"
const defaultPort = "8081"

func installDir() string {
	if d := os.Getenv("RUNNER_INSTALL_DIR"); d != "" {
		return d
	}
	return defaultInstallDir
}

func runScriptName() string {
	if runtime.GOOS == "windows" {
		return "run.cmd"
	}
	return "run.sh"
}

func getStatus(installDir string) (status string, running bool) {
	if installDir == "" {
		return "missing", false
	}
	fi, err := os.Stat(installDir)
	if err != nil || !fi.IsDir() {
		return "missing", false
	}
	runnerFile := filepath.Join(installDir, ".runner")
	if _, err := os.Stat(runnerFile); err != nil {
		return "new", false
	}
	// Don't look for a pid file: actions/runner doesn't write one (see the package comment in internal/runnerproc),
	// so the lookup always fails, every runner reports "not running", and the Manager relaunches it every 5 minutes.
	return "installed", runnerproc.Running(installDir)
}

// startMu serializes the check-then-start steps of /start, see handleStart
var startMu sync.Mutex

func startRunner(installDir string) error {
	script := filepath.Join(installDir, runScriptName())
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("%s not found: %w", script, err)
	}
	cmd := exec.Command(script)
	cmd.Dir = installDir
	// run.sh runs the user's Job: the AGENT_TOKEN injected into the Agent must not be passed down, see internal/childenv
	cmd.Env = childenv.Environ()
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// stopRunner sends SIGTERM to all Runner processes under this install directory.
// runnerproc.Find places run.sh before Runner.Listener, so the supervisor script is stopped first,
// otherwise its while loop would start a new listener after the old one exits.
func stopRunner(installDir string) error {
	pids := runnerproc.Find(installDir)
	if len(pids) == 0 {
		return fmt.Errorf("no running runner process found under %s", installDir)
	}
	var firstErr error
	for _, pid := range pids {
		process, err := os.FindProcess(pid)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// A process may exit between the scan and the signal; that is not a failure
		if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

type statusResponse struct {
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := installDir()
	status, running := getStatus(dir)
	_ = json.NewEncoder(w).Encode(statusResponse{Status: status, Running: running})
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := installDir()
	// The "is it running?" check and the launch must be one atomic section: the Manager's periodic checks, the
	// start after registration completes, and UI clicks can all hit this at once, and if each sees "not running" each would launch a run.sh.
	startMu.Lock()
	defer startMu.Unlock()
	if _, run := getStatus(dir); run {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"already running"}`))
		return
	}
	if err := startRunner(dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"message":"started"}`))
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := installDir()
	if err := stopRunner(dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"message":"stop signal sent"}`))
}

// agentTokenFile must match runner.AgentTokenFile on the Manager side:
// the Manager writes the token to the mounted directory, and the Agent reads it from its own RUNNER_INSTALL_DIR.
const agentTokenFile = ".agent_token"

// expectedToken returns the token this Agent requires: the environment variable first, then the token file in the install directory.
// An empty string means the credential is unavailable; control endpoints must reject requests.
func expectedToken() string {
	// Environment variable first: the Manager injects it when creating the container, so it doesn't depend on the mounted file being readable by this UID.
	if t := strings.TrimSpace(os.Getenv("AGENT_TOKEN")); t != "" {
		return t
	}
	path := filepath.Join(installDir(), agentTokenFile)
	b, err := os.ReadFile(path)
	if err != nil {
		// If the file is missing or unreadable, the read failure must not be interpreted as "no authentication needed".
		if !os.IsNotExist(err) {
			logTokenUnreadable(path, err)
		}
		return ""
	}
	return strings.TrimSpace(string(b))
}

// tokenWarnOnce ensures the permission warning is logged only once, so status polling doesn't flood the log
var tokenWarnOnce sync.Once

func logTokenUnreadable(path string, err error) {
	tokenWarnOnce.Do(func() {
		log.Printf("warning: the token file %s exists but cannot be read (%v), so the endpoints reject requests. "+
			"This usually means the Manager and the Agent run under different UIDs; recreate the runner container "+
			"to have the token injected through the environment instead", path, err)
	})
}

// bearerPrefix is the auth scheme in the Authorization header; per RFC 7235 the scheme name is case-insensitive
const bearerPrefix = "Bearer "

// bearerToken extracts the token from `Authorization: Bearer <token>`, and returns an empty string if the scheme is not Bearer.
//
// This used to use strings.TrimPrefix, which returns the whole string unchanged when the prefix is missing, so a
// bare token with no scheme would also pass. That isn't a vulnerability (the caller still needs the secret string),
// but all six language docs say `Authorization: Bearer`, and the Manager has always sent Bearer
// (see runner.setAgentAuth): the code was looser than its own documentation, with no good reason.
func bearerToken(header string) string {
	h := strings.TrimSpace(header)
	if len(h) < len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(h[len(bearerPrefix):])
}

// requireToken wraps the control endpoints; it fails closed when the token is unavailable, otherwise it requires a matching Bearer token.
// The token is re-read on every request, for the case where the Manager writes it after the container has started.
func requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := expectedToken()
		if want == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"agent authentication unavailable"}`))
			return
		}
		got := bearerToken(r.Header.Get("Authorization"))
		if !secure.ConstantTimeEqual(got, want) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
			return
		}
		next(w, r)
	}
}

// newAgentMux registers all of the Agent's routes.
//
// Use a dedicated mux instead of http.DefaultServeMux: DefaultServeMux is a package-level global,
// and registering the same path twice in one process panics, so tests could only start one server. With a separate mux
// newAgentServer can be constructed repeatedly, which also makes the timeout fields testable.
func newAgentMux() *http.ServeMux {
	mux := http.NewServeMux()
	// Control endpoints require the token; /health stays open for the container HEALTHCHECK
	mux.HandleFunc("/status", requireToken(handleStatus))
	mux.HandleFunc("/start", requireToken(handleStart))
	mux.HandleFunc("/stop", requireToken(handleStop))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// newAgentServer builds the HTTP server with timeouts.
//
// An http.Server without timeouts waits indefinitely on a peer that has stopped talking: a half-open TCP connection
// can hold a goroutine and a file descriptor until the process exits, and the Runner container is long-lived.
// Every Agent handler returns immediately (looks up the process table, sends signals), so the timeouts can be tight.
func newAgentServer(port string, mux *http.ServeMux) *http.Server {
	return &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Only the Manager connects, and its requests are at most an empty POST
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// shutdown gives a running Job a chance to finish, then closes the HTTP server.
//
// This is the link where docker stop -t <grace period> actually reaches the Runner. The Agent is PID 1 in the container
// (forwarding through tini), so SIGTERM arrives here first; if it exits immediately as it used to, the kernel would
// Kill everything left in the PID namespace with SIGKILL. run.sh and Runner.Listener
// do not receive SIGTERM, so the Job is not canceled gracefully and GitHub can only wait for the "lost communication" timeout.
//
// Returns true if the Runner exited cleanly within the grace period.
func shutdown(ctx context.Context, dir string, grace time.Duration) bool {
	if !runnerproc.Running(dir) {
		return true
	}
	// stopRunner sends SIGTERM in the order "supervisor script before listener". This order must be kept:
	// if the listener is stopped first, the while loop in run.sh immediately starts a new one.
	if err := stopRunner(dir); err != nil {
		log.Printf("sending SIGTERM to the runner failed: %v", err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !runnerproc.Running(dir) {
			return true
		}
		select {
		case <-ctx.Done():
			return !runnerproc.Running(dir)
		case <-time.After(runnerPollInterval):
		}
	}
	return !runnerproc.Running(dir)
}

// runnerPollInterval is the polling interval used while waiting for the Runner to exit.
// Each poll scans procfs, so 200ms is fast enough (relative to a grace period of tens of seconds)
// without competing with the Job for CPU in a container that is busy shutting down.
const runnerPollInterval = 200 * time.Millisecond

func main() {
	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = defaultPort
	}
	srv := newAgentServer(port, newAgentMux())
	authState := "enabled"
	if expectedToken() == "" {
		authState = "unavailable (control requests rejected until credentials are restored)"
	}
	log.Printf("Runner Agent listening on :%s, RUNNER_INSTALL_DIR=%s, endpoint auth %s", port, installDir(), authState)

	// Subscribe before listening: if docker stop arrives right after the container starts, the signal
	// must not fall through to the default behavior for unsubscribed signals. The Go runtime terminates the process immediately on an unhandled SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, os.Interrupt)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-quit
	grace := runnerproc.StopGracePeriod - runnerproc.ShutdownReserve
	log.Printf("received SIGTERM, stopping the runner (up to %s)", grace)
	if shutdown(context.Background(), installDir(), grace) {
		log.Print("runner stopped")
	} else {
		log.Printf("runner still running after %s, exiting anyway", grace)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), runnerproc.ShutdownReserve)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutting down the HTTP server failed: %v", err)
	}
	os.Exit(0)
}
