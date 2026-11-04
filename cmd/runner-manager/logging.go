package main

import (
	"io"
	"log"
	"os"
	"strings"

	"github.com/labstack/echo/v4"
	logkit "github.com/soulteary/logger-kit/v3"
)

// serviceName is written into the service field of every log line.
const serviceName = "runner-deck"

// setupLogging sets up logging: environment variables choose the level and format, and it takes over the standard library log output.
//
// Taking over log is the key step. The repo still has 34 log.Printf calls; if only the request log were switched to
// logger-kit, the two formats would alternate in the same output: awkward for people to read and even harder for machines to parse.
// zerolog.Logger itself implements io.Writer, so passing it to log.SetOutput is enough,
// and those 34 calls would use the same format and level without changing a single line.
//
// Default to console instead of kit's default JSON: the current output is meant for people to read,
// switching the default to JSON would first degrade the experience for everyone reading logs in a terminal.
// Where JSON is needed (collected into ELK/Loki), set LOG_FORMAT=json.
func setupLogging() *logkit.Logger { return setupLoggingTo(os.Stderr) }

// setupLoggingTo is the testable form of setupLogging: the caller supplies the output target.
func setupLoggingTo(out io.Writer) *logkit.Logger {
	cfg := logkit.Config{
		Level:       parseLogLevel(os.Getenv("LOG_LEVEL")),
		Format:      parseLogFormat(os.Getenv("LOG_FORMAT")),
		Output:      out,
		ServiceName: serviceName,
	}
	lg := logkit.New(cfg)
	logkit.SetDefault(lg)

	// The standard log package adds its own timestamp prefix, and zerolog adds one too. Drop the former,
	// otherwise every line reads "time time message".
	log.SetFlags(0)
	log.SetOutput(stdlogWriter{lg})
	return lg
}

// stdlogWriter treats each line written by the standard log package as one Info record.
//
// Don't pass lg.Zerolog() directly to log.SetOutput: zerolog.Logger does implement io.Writer,
// but that path carries no level. In console format it prints "???", and JSON output has no level field,
// so any place that filters logs by level would silently drop all 34 lines.
type stdlogWriter struct{ lg *logkit.Logger }

func (w stdlogWriter) Write(p []byte) (int, error) {
	// log.Printf messages come with a trailing newline, and zerolog adds its own, so keeping it produces blank lines
	w.lg.Info().Msg(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func parseLogLevel(v string) logkit.Level {
	if lvl, err := logkit.ParseLevel(strings.TrimSpace(v)); err == nil {
		return lvl
	}
	return logkit.InfoLevel
}

// An unrecognized value falls back to console instead of exiting with an error: a misconfigured log format should not keep the service from starting.
func parseLogFormat(v string) logkit.Format {
	if strings.EqualFold(strings.TrimSpace(v), "json") {
		return logkit.FormatJSON
	}
	return logkit.FormatConsole
}

// requestLogger is the request logging middleware.
//
// Replacing Echo's built-in middleware.RequestLogger() gives us: each request log carries a
// real request_id (Echo's version always prints request_id=""), and the query string and request headers
// sensitive fields are redacted by logger-kit according to its established rules.
//
// The logger-kit v3 root package only accepts net/http, so here it is adapted via echo.WrapMiddleware.
func requestLogger() echo.MiddlewareFunc {
	cfg := logkit.DefaultMiddlewareConfig()
	cfg.Logger = logkit.Default()
	// Probes and scrapes run every few seconds; logging them would only bury the genuinely useful lines.
	// metricsPath is the /metrics endpoint added in this PR. Prometheus scrapes it every 15s by default,
	// so unless it is skipped, most lines in the request log would come from it.
	cfg.SkipPaths = []string{"/health", "/ready", metricsPath}
	return echo.WrapMiddleware(logkit.Middleware(cfg))
}
