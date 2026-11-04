// Package childenv constructs the environment variables for child processes spawned by Manager and Agent.
//
// Child processes (run.sh and the Jobs it runs, config.sh, install-runner.sh) must not receive
// the Manager's or Agent's own credentials. Previously all four sites were cmd.Env = os.Environ(), so in default mode
// docker-compose.yml injected the Manager's BASIC_AUTH_PASSWORD via environment:, which propagated down to the
// Job environment. A workflow env, printenv, or set step, or a third-party Action that prints environment variables,
// could write the admin panel password into the Job log. It is not an Actions secret, so GitHub does not mask it.
// Container mode is the same: AGENT_TOKEN, which Manager injects into Agent with -e, ends up in the Job environment.
//
// Why a denylist rather than an allowlist: in the environment passed down, most variables are ones the Job needs.
// In default mode with DinD, the daemon is found via DOCKER_HOST inherited from the Manager. Users also often set
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY, RUNNER_ALLOW_RUNASROOT, TZ, LANG, and a number of RUNNER_* and ACTIONS_*
// that actions/runner recognizes. If an allowlist misses any of these, it fails silently:
// the Job does not say "a variable is missing"; it just fails in a way whose cause is hard to see. If a denylist misses a variable,
// the cost is that it keeps leaking, and that class is watched by guard_test.go: a new credential-type variable not in Denied makes the test fail.
//
// This blocks passive leaks (logs, third-party Actions, error reporting), not an isolation boundary. In default mode the Job and
// Manager both run as UID 1001 and can read /proc/<manager pid>/environ directly; the image also has passwordless sudo
// and docker.sock. Blocking a Job that actively goes after credentials requires changing the trust boundary, not this code (see SECURITY.md).
package childenv

import (
	"os"
	"strings"
)

// Denied lists the variable names that must never appear in a child process environment. Matching is exact and case-sensitive.
//
// BASIC_AUTH_USER is not a secret, only the password is, but it also belongs only to Manager and is useless to a Job,
// so it is removed too, and what leaks is not "half a credential". Note that the PASSWORD|TOKEN|SECRET|_KEY$
// pattern in guard_test.go does not match this name; it was added deliberately, not found by pattern scanning.
var Denied = []string{
	"BASIC_AUTH_PASSWORD",
	"BASIC_AUTH_USER",
	"AGENT_TOKEN",
}

// Sanitize returns a copy of environ with the Denied entries removed, keeping the remaining entries in original order; it does not modify the input.
//
// Keys are the part before the first =, matched exactly: BASIC_AUTH_PASSWORD_FILE and XAGENT_TOKEN must both be kept,
// because prefix matching would delete them too. Malformed entries without = have no key and are kept as-is; execve does not
// care what shape this entry is either, so there is no reason to override it here.
func Sanitize(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if ok && isDenied(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isDenied(key string) bool {
	for _, name := range Denied {
		if key == name {
			return true
		}
	}
	return false
}

// Environ equals Sanitize(os.Environ()), for direct use as exec.Cmd.Env.
//
// It only affects the copy handed to child processes; this process's environment is untouched. Agent calls
// os.Getenv("AGENT_TOKEN") on every request to authenticate (see expectedToken in cmd/runner-agent),
// so calling Unsetenv on itself would disable authentication for /start and /stop.
func Environ() []string {
	return Sanitize(os.Environ())
}
