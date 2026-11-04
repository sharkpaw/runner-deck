package childenv

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSanitize_RemovesEveryDeniedName Every item in Denied must be removed.
//
// Iterate over Denied instead of hardcoding the three names: if a variable is added to Denied later, this test covers it automatically,
// so there is no case where an entry gets added to the list but nobody verifies it is actually removed.
func TestSanitize_RemovesEveryDeniedName(t *testing.T) {
	for _, name := range Denied {
		t.Run(name, func(t *testing.T) {
			in := []string{"PATH=/usr/bin", name + "=leaked", "HOME=/home/app"}
			got := Sanitize(in)
			if slices.Contains(got, name+"=leaked") {
				t.Errorf("%s still in result: %v", name, got)
			}
			if want := []string{"PATH=/usr/bin", "HOME=/home/app"}; !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

// TestSanitize_KeepsEverythingElse All other variables must pass through unchanged.
//
// These were not picked at random; each accidental removal has a concrete consequence: without DOCKER_HOST, a Job in container mode with DinD
// cannot connect to the daemon; without PATH/HOME, run.sh fails to start; without proxy variables, action downloads hang in intranet deployments;
// without RUNNER_ALLOW_RUNASROOT, a runner running as root refuses to start.
// Together they are the evidence for why this is a denylist rather than an allowlist.
func TestSanitize_KeepsEverythingElse(t *testing.T) {
	keep := []string{
		"DOCKER_HOST=tcp://runner-dind:2375",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/home/app",
		"HTTPS_PROXY=http://proxy.internal:3128",
		"RUNNER_ALLOW_RUNASROOT=1",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/hook.sh",
	}
	got := Sanitize(slices.Clone(keep))
	if !slices.Equal(got, keep) {
		t.Errorf("got %v, want %v", got, keep)
	}
}

// TestSanitize_MatchesNamesExactly Matching is exact, not by prefix.
//
// If the equality check in Sanitize is replaced with strings.HasPrefix, this test turns red, which is exactly the bug this guards against:
// BASIC_AUTH_PASSWORD_FILE is a common "password read from file" convention, and XAGENT_TOKEN could be any user's own variable;
// neither is related to this repository's credentials, and a prefix match would swallow both.
func TestSanitize_MatchesNamesExactly(t *testing.T) {
	in := []string{
		"BASIC_AUTH_PASSWORD_FILE=/run/secrets/pw",
		"XAGENT_TOKEN=y",
		"AGENT_TOKEN_PATH=/run/secrets/tok",
		"BASIC_AUTH_PASSWORD=should-go",
	}
	got := Sanitize(in)
	want := []string{
		"BASIC_AUTH_PASSWORD_FILE=/run/secrets/pw",
		"XAGENT_TOKEN=y",
		"AGENT_TOKEN_PATH=/run/secrets/tok",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSanitize_KeepsOddlyShapedEntries Entries whose value contains =, whose value is empty, or that contain no = at all are all kept as-is.
//
// environ does not guarantee every entry looks like KEY=VALUE: a value containing = is perfectly valid (A=b=c has key A and value b=c),
// KEY= is a variable that exists but is empty, and an entry without = has no key at all. As long as the key does not match Denied,
// we do not make decisions on behalf of execve here.
func TestSanitize_KeepsOddlyShapedEntries(t *testing.T) {
	in := []string{"A=b=c", "EMPTY=", "NOEQUALSIGN", "AGENT_TOKEN=go-away"}
	got := Sanitize(in)
	want := []string{"A=b=c", "EMPTY=", "NOEQUALSIGN"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSanitize_PreservesOrderOfRemainingEntries The remaining entries keep their original order.
//
// Order is not pedantry: environ may contain duplicate variable names, and which one wins is decided by libc (glibc takes the first).
// Reordering would silently change the value of duplicated variables.
func TestSanitize_PreservesOrderOfRemainingEntries(t *testing.T) {
	in := []string{"Z=1", "AGENT_TOKEN=t", "M=2", "BASIC_AUTH_USER=admin", "A=3", "DUP=first", "DUP=second"}
	got := Sanitize(in)
	want := []string{"Z=1", "M=2", "A=3", "DUP=first", "DUP=second"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSanitize_DoesNotModifyItsInput The input slice is unchanged after the call.
//
// os.Environ() returns a new slice on each call, but it has more than one caller (runInstallRunnerScript also
// appends an entry), and filtering in place and truncating would corrupt the underlying array. Someone has to make sure Sanitize returns a new slice.
func TestSanitize_DoesNotModifyItsInput(t *testing.T) {
	in := []string{"PATH=/usr/bin", "AGENT_TOKEN=t", "HOME=/home/app"}
	before := slices.Clone(in)
	_ = Sanitize(in)
	if !slices.Equal(in, before) {
		t.Errorf("input was modified to %v, originally %v", in, before)
	}
}

// TestSanitize_EmptyAndNil Empty input must not panic, and must not return anything other than nil or an empty result.
func TestSanitize_EmptyAndNil(t *testing.T) {
	if got := Sanitize(nil); len(got) != 0 {
		t.Errorf("Sanitize(nil) = %v, expected empty", got)
	}
	if got := Sanitize([]string{}); len(got) != 0 {
		t.Errorf("Sanitize([]) = %v, expected empty", got)
	}
}

// TestEnviron_FiltersTheProcessEnvironment Environ is Sanitize(os.Environ()),
// and does not touch this process's environment: the agent reads AGENT_TOKEN from the environment on every request to authenticate.
func TestEnviron_FiltersTheProcessEnvironment(t *testing.T) {
	t.Setenv("AGENT_TOKEN", "super-secret")
	t.Setenv("DOCKER_HOST", "tcp://d:2375")

	got := Environ()
	for _, entry := range got {
		if k, _, _ := strings.Cut(entry, "="); k == "AGENT_TOKEN" {
			t.Errorf("Environ() still contains %q", entry)
		}
	}
	if !slices.Contains(got, "DOCKER_HOST=tcp://d:2375") {
		t.Error("Environ() dropped DOCKER_HOST")
	}
	if os.Getenv("AGENT_TOKEN") != "super-secret" {
		t.Error("Environ() modified this process's environment; the agent's authentication reads its token from here")
	}
}
