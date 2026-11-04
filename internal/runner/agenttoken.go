// Agent call token: the Agent inside the Runner container exposes control endpoints such as /start and /stop,
// reachable from any container on the same docker network. The token confirms the caller really is the Manager.
//
// The token is stored in the Runner install directory, which the Manager accesses (host-side path)
// and which is also mounted into the Runner container (/runner). So it does not need to be passed through environment variables,
// and the Manager can read the same token again after a restart.
package runner

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	secure "github.com/soulteary/secure-kit/v2"
)

// AgentTokenFile is the name of the file in the Runner install directory that stores the Agent token
const AgentTokenFile = ".agent_token"

// agentTokenBytes is the number of random bytes in the token; it is 64 characters after hex encoding
const agentTokenBytes = 32

// EnsureAgentToken returns the Agent token for this Runner: it reads the token if it exists, otherwise it generates one and writes it with mode 0600.
// On a generation or write failure it returns an error; callers must stop any operation that requires authentication.
//
// The start/stop path (holds the runnerOps lock) and the status path (no lock, because it runs on every list and must not be blocked by a
// docker create that takes a few seconds) both reach here, so creation must have a single winner: if each generated its own,
// whichever was written to disk last would overwrite the first, yet the value injected into the container is the first one. The Agent checks its env A,
// the Manager later reads B from disk and gets 401 forever, and the container env is non-empty, so the agent_token drift check cannot repair it either.
//
// The approach is to write a temp file first and, once the content is complete, atomically attach the final name. os.Link does not overwrite an existing name,
// so it gives a single winner and also guarantees that the .agent_token name either does not exist or has complete content.
// There is no intermediate state of 'created but not yet written'.
//
// Empty files are deliberately not reclaimed. Reclaiming requires unlink, and the idea that 'if it is still empty after a few milliseconds, the writer is dead'
// does not hold. The writer may simply have been descheduled, or the filesystem write may have stalled (host load,
// NFS, or cgroup IO throttling can all cause this). Unlinking then removes the inode of a writer that is still alive,
// which still writes A into the now-nameless inode and returns A, while the disk keeps B, published by the reclaimer.
// That is exactly the split described above. Since this implementation never produces empty files, there is nothing to reclaim.
func EnsureAgentToken(installDir string) (string, error) {
	if token := ReadAgentToken(installDir); token != "" {
		return token, nil
	}
	path := filepath.Join(installDir, AgentTokenFile)

	// RandomHex(n) returns 2n hex characters, equivalent to the original rand.Read(32 bytes) plus hex encoding
	token, err := secure.RandomHex(agentTokenBytes)
	if err != nil {
		return "", fmt.Errorf("cannot generate the Agent token: %w", err)
	}

	// os.CreateTemp creates the file with 0600, consistent with the final file
	f, err := os.CreateTemp(installDir, AgentTokenFile+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("cannot create the temporary token file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write([]byte(token)); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("cannot write the temporary token file %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("cannot write the temporary token file %s: %w", tmp, err)
	}

	if err := os.Link(tmp, path); err != nil {
		if !os.IsExist(err) {
			return "", fmt.Errorf("cannot publish the Agent token %s: %w", path, err)
		}
		// The name is already taken: someone else published first, so use theirs. Content attached via Link is always complete,
		if existing := ReadAgentToken(installDir); existing != "" {
			return existing, nil
		}
		// so the occupying file is one whose content cannot be read. This implementation does not produce such a file, so it is either a leftover
		// from an earlier version's failed write, or a permissions problem. Neither should be deleted from here (see the note above about
		// unlink). Hand it to a human and state clearly what should be done.
		return "", fmt.Errorf("the token file %s exists but reads back empty (a leftover from a failed write by an earlier version, "+
			"or the permissions are wrong). Restore the existing token and its permissions before retrying; "+
			"the Manager will not make unauthenticated Agent calls", path)
	}
	return token, nil
}

// tokenWarnOnce warns only once per install directory, to avoid flooding the log from status polling
var tokenWarnOnce sync.Map

// WarnAgentTokenUnavailable warns when the token cannot be obtained. The status path calls EnsureAgentToken on every list,
// so it cannot warn every time. But it must not stay silent either: without a token the Manager cannot call the Agent,
// and the agent_token drift check skips itself because it has no token, so nothing shows up in the UI.
func WarnAgentTokenUnavailable(installDir string, err error) {
	if err == nil {
		return
	}
	v, ok := tokenWarnOnce.Load(installDir)
	if !ok {
		v, _ = tokenWarnOnce.LoadOrStore(installDir, &sync.Once{})
	}
	v.(*sync.Once).Do(func() {
		log.Printf("warning: %v", err)
	})
}

// ReadAgentToken reads the token. It returns an empty string if the file does not exist or is empty.
// An empty string means the credential is unavailable and must not be treated as allowing anonymous access.
func ReadAgentToken(installDir string) string {
	b, err := os.ReadFile(filepath.Join(installDir, AgentTokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
