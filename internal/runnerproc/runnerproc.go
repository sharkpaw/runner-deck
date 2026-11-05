// Package runnerproc determines whether an actions/runner process installed under a given directory is alive.
//
// Why not read a pid file: actions/runner never writes one. None of the three startup scripts
// (src/Misc/layoutroot/run.sh, src/Misc/layoutroot/run-helper.sh.template,
// src/Misc/layoutbin/runsvc.sh) contains a statement that writes a pid file. The pid only lives in the shell
// variable $PID; the .path file in the install directory holds a PATH string, not a pid. runsvc.sh
// even does `export PATH=$(cat .path)`.
//
// The earlier approach, which read the pid from "Runner.Listener.pid" / ".path", therefore always returned "not found" for
// every real runner directory. As a result every runner was judged not running, and the Manager
// restarted each one again on its periodic 5-minute health check.
//
// It now scans procfs and claims processes by the absolute path in their argv. The scanning itself is handled by procfind-kit;
// this package keeps only the actions/runner-specific knowledge: which scripts and which executable,
// and why both must be matched (see runnerSpec).
//
// Linux reads procfs and macOS reads sysctl (scan_darwin.go). Other platforms always return "not found".
package runnerproc

import (
	"path/filepath"
	"runtime"
	"time"

	procfind "github.com/soulteary/procfind-kit"
)

// StopGracePeriod is the grace period docker stop gives a container: the -t argument when the Manager stops a Runner container,
// and also stop_grace_period for runner-manager in docker-compose.yml.
//
// PID 1 receiving SIGTERM (the Agent in container mode, the Manager in default mode) waits at most
// StopGracePeriod - ShutdownReserve for the Runner to exit, leaving the rest for its own cleanup.
// These must be changed together, so the grace period is defined only here: if the two sides disagree, the shorter one wins.
// If -t is shorter than the time the Agent waits, the Agent is still waiting on the Runner and the container gets SIGKILLed as a whole.
const StopGracePeriod = 30 * time.Second

// ShutdownReserve is the margin PID 1 keeps for itself within the grace period: shutting down the HTTP server and writing the final log line.
// Without this margin it would wait until docker stop's timer runs out and then be SIGKILLed itself,
// so the "runner has not exited yet" log line would never get printed, and that is the only clue during troubleshooting.
const ShutdownReserve = 5 * time.Second

// procRoot is the procfs mount point, made a variable only so tests can replace it.
var procRoot = defaultProcRoot

const defaultProcRoot = "/proc"

// processScanner finds a program's processes by the paths in their argv.
type processScanner interface {
	Find(dir string, spec procfind.Spec) []int
	FindMany(dirs []string, spec procfind.Spec) map[string][]int
	Running(dir string, spec procfind.Spec) bool
}

func procfsScanner() processScanner { return procfind.Scanner{Root: procRoot} }

// listenerName is the path of the listener executable relative to the install directory.
// run-helper.sh launches it as `"$DIR"/bin/Runner.Listener run $*`, so argv[0] is that path.
func listenerName() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "Runner.Listener.exe")
	}
	return filepath.Join("bin", "Runner.Listener")
}

// ListenerBinary returns the absolute path of the Runner.Listener executable in this install directory.
func ListenerBinary(installDir string) string {
	return filepath.Join(installDir, listenerName())
}

// runnerSpec describes what counts as a runner process belonging to this install directory.
//
// Paths are relative to the install directory; procfind-kit builds the absolute paths and matches them against argv.
//
// Matching the supervisor scripts and not only the listener is deliberate: run.sh waits between listener runs:
// on exit code 2, run-helper.sh runs `safe_sleep.sh 5` and then returns, and run.sh's
// while loop starts it again. During those 5 seconds the listener process really does not exist. If only the listener were matched,
// the runner would be judged dead, and the Manager would start another one, producing exactly the duplicate starts this package exists to eliminate.
//
// Splitting them into Scripts and Executables is not arbitrary: procfind guarantees scripts are returned before
// executables, and Stop depends on that order: let run.sh exit first,
// otherwise its while loop could restart the listener one more time right at that moment.
func runnerSpec() procfind.Spec {
	if runtime.GOOS == "windows" {
		return procfind.Spec{
			Scripts:     []string{"run.cmd", "run-helper.cmd"},
			Executables: []string{listenerName()},
		}
	}
	return procfind.Spec{
		Scripts:     []string{"run.sh", "run-helper.sh"},
		Executables: []string{listenerName()},
	}
}

// Find returns the runner process pids belonging to installDir, with the supervisor scripts first and the listener after.
func Find(installDir string) []int {
	return scanner().Find(installDir, runnerSpec())
}

// FindMany scans procfs once, dispatches processes to their install directories, and returns results indexed by the original input strings.
// Equivalent to calling Find for each directory, except N directories are scanned only once. List needs to judge a whole batch of
// runners at once; scanning one at a time would mean reading the entire process table N times.
func FindMany(installDirs []string) map[string][]int {
	return scanner().FindMany(installDirs, runnerSpec())
}

// Running reports whether a runner process is alive under installDir
func Running(installDir string) bool {
	return scanner().Running(installDir, runnerSpec())
}

// WorkerRunning reports whether a job is running under installDir: the listener starts one
// Runner.Worker per job and it exits with the job.
func WorkerRunning(installDir string) bool {
	name := "Runner.Worker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return scanner().Running(installDir, procfind.Spec{Executables: []string{filepath.Join("bin", name)}})
}
