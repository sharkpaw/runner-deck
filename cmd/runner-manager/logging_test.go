package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	logkit "github.com/soulteary/logger-kit/v3"
)

// syncBuf: log output may be written from request-handling goroutines; without a lock, -race reports a data race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Install the logger and send one request, then return the captured output.
func captureLog(t *testing.T, format, target string, status int) string {
	t.Helper()
	t.Setenv("LOG_FORMAT", format)
	t.Setenv("LOG_LEVEL", "debug")

	out := &syncBuf{}
	setupLoggingTo(out)
	t.Cleanup(restoreStdlog)

	e := echo.New()
	e.Use(requestLogger())
	e.Any("/*", func(c echo.Context) error { return c.String(status, "body") })
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	return out.String()
}

// The main reason to replace Echo's built-in RequestLogger: it logs the full uri as-is,
// so token values in the query string get written to disk in plain text. logger-kit redacts them per its established rules.
func TestRequestLogger_RedactsSensitiveQueryParams(t *testing.T) {
	got := captureLog(t, "json", "/api/runners?token=SECRET123&q=ok", http.StatusOK)
	if strings.Contains(got, "SECRET123") {
		t.Errorf("Sensitive query parameter was written to the log in plain text:\n%s", got)
	}
	if !strings.Contains(got, "token=***") {
		t.Errorf("Expected token to be replaced with ***, actual:\n%s", got)
	}
	// Non-sensitive parameters must not be redacted by mistake
	if !strings.Contains(got, "q=ok") {
		t.Errorf("Ordinary query parameter should not be redacted:\n%s", got)
	}
}

// Echo's RequestLogger always prints request_id="". It only reads the X-Request-Id request header,
// and leaves it empty when absent. logger-kit generates one itself, which is what makes it possible to correlate a single request's logs.
func TestRequestLogger_AlwaysHasRequestID(t *testing.T) {
	var m map[string]any
	line := firstJSONLine(t, captureLog(t, "json", "/api/runners", http.StatusOK))
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("Not JSON: %v\n%s", err, line)
	}
	id, _ := m["request_id"].(string)
	if id == "" {
		t.Errorf("request_id is empty; log lines cannot be correlated:\n%s", line)
	}
}

// The most common problem when adapting net/http middleware with echo.WrapMiddleware:
// the status code is read from the wrapped ResponseWriter; if that is wired wrong, it will always be logged as 200.
func TestRequestLogger_CapturesRealStatusCode(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTeapot, http.StatusInternalServerError} {
		line := firstJSONLine(t, captureLog(t, "json", "/api/runners", status))
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("Not JSON: %v\n%s", err, line)
		}
		if got := int(m["status"].(float64)); got != status {
			t.Errorf("Logged status code %d, actual %d", got, status)
		}
	}
}

// Health probes and Prometheus scrapes run every few seconds; logging them would drown out the useful lines.
//
// /metrics only became a problem after two PRs were merged: the logger originally skipped just two probes,
// but Prometheus scrapes /metrics every 15s by default, so without skipping it, most request log lines would be from it.
func TestRequestLogger_SkipsProbeEndpoints(t *testing.T) {
	for _, p := range []string{"/health", "/ready", metricsPath} {
		if got := captureLog(t, "json", p, http.StatusOK); strings.Contains(got, `"path":"`+p+`"`) {
			t.Errorf("%s should not appear in the request log:\n%s", p, got)
		}
	}
	// Reverse check: ordinary paths must be logged
	if got := captureLog(t, "json", "/api/runners", http.StatusOK); !strings.Contains(got, `"path":"/api/runners"`) {
		t.Errorf("Ordinary path should be logged:\n%s", got)
	}
}

// The repo still has 34 log.Printf calls. They must use the same format and level as the request log,
// otherwise two formats alternate in the same output and machines cannot parse it.
func TestStdlibLogGoesThroughTheSameLogger(t *testing.T) {
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("LOG_LEVEL", "debug")
	out := &syncBuf{}
	setupLoggingTo(out)
	t.Cleanup(restoreStdlog)

	log.Printf("配置已加载: %s", "config.yaml")

	line := firstJSONLine(t, out.String())
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log.Printf did not use JSON format: %v\n%s", err, line)
	}
	// Key point: a level is required. If lg.Zerolog() is passed directly to log.SetOutput, the level is missing here,
	// under console it shows as "???", and these 34 lines would be entirely dropped when filtering by level.
	if m["level"] != "info" {
		t.Errorf("level = %v, expected info (the reason stdlogWriter exists): %s", m["level"], line)
	}
	if m["message"] != "配置已加载: config.yaml" {
		t.Errorf("message = %v", m["message"])
	}
	if m["service"] != serviceName {
		t.Errorf("service = %v, expected %s", m["service"], serviceName)
	}
	// The trailing newline that log.Printf adds should not turn into an empty line in the log
	if s, _ := m["message"].(string); strings.HasSuffix(s, "\n") {
		t.Errorf("message has an extra trailing newline: %q", s)
	}
}

// Keep console by default: current output is meant to be read by people in a terminal,
// switching to JSON by default would make everyone who reads the logs take a regression first. JSON is opt-in.
func TestParseLogFormat_DefaultsToConsole(t *testing.T) {
	cases := []struct {
		in   string
		want logkit.Format
	}{
		{"", logkit.FormatConsole},
		{"console", logkit.FormatConsole},
		{"json", logkit.FormatJSON},
		{"JSON", logkit.FormatJSON},
		{"  json  ", logkit.FormatJSON},
		{"什么鬼", logkit.FormatConsole}, // A misconfiguration should not prevent the service from starting
	}
	for _, tc := range cases {
		if got := parseLogFormat(tc.in); got != tc.want {
			t.Errorf("parseLogFormat(%q) = %v, expected %v", tc.in, got, tc.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want logkit.Level
	}{
		{"", logkit.InfoLevel},
		{"debug", logkit.DebugLevel},
		{"warn", logkit.WarnLevel},
		{"error", logkit.ErrorLevel},
		{"  debug  ", logkit.DebugLevel},
		{"什么鬼", logkit.InfoLevel}, // Same as above: fall back instead of erroring
	}
	for _, tc := range cases {
		if got := parseLogLevel(tc.in); got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, expected %v", tc.in, got, tc.want)
		}
	}
}

// restoreStdlog resets the standard library log.
//
// It must reset to os.Stderr rather than nil: setupLoggingTo changes process-wide global state,
// and the other 34 log.Printf calls in the repo still write after the test ends. Passing nil would panic,
// showing up as "passes when run alone, fails on another test in the full run".
func restoreStdlog() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
}

// Take the first output line that parses as JSON.
func firstJSONLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) == nil {
			return line
		}
	}
	t.Fatalf("no JSON line in output:\n%s", out)
	return ""
}
