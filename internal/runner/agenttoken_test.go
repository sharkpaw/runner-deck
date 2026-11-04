package runner

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestEnsureAgentToken_SingleWinnerUnderConcurrency
// The start/stop path (holds the runnerOps lock) and the status path (no lock, since it runs on every listing and must not be blocked by a
// several-second docker create) both reach EnsureAgentToken. "Read first, then write" is not atomic: each side generates one,
// and the later write overwrites the earlier one. The token injected into the container is the earlier one, so the Agent accepts A from env while the Manager reads B from disk,
// and every request fails with 401 from then on. The container env is non-empty, so the agent_token drift check cannot fix it either; only manual intervention helps.
// So this requires a "single winner": no matter how many calls run concurrently, everyone must get the same token, and it must match what is on disk.
func TestEnsureAgentToken_SingleWinnerUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	tokens := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // Try to make them enter at the same time
			tok, err := EnsureAgentToken(dir)
			if err != nil {
				t.Errorf("EnsureAgentToken failed on concurrent call %d: %v", i, err)
				return
			}
			tokens[i] = tok
		}(i)
	}
	close(start)
	wg.Wait()

	onDisk := ReadAgentToken(dir)
	if onDisk == "" {
		t.Fatal("no token on disk after concurrent calls finished")
	}
	for i, tok := range tokens {
		if tok != onDisk {
			t.Fatalf("concurrent call %d got %q, but disk holds %q - what is injected into the container will not match what the Manager reads later, and the Agent will return 401 forever",
				i, tok, onDisk)
		}
	}
}

// TestEnsureAgentToken_ReusesExistingToken must return an existing token unchanged when one is present, not regenerate it:
// Regenerating would mean already-running containers (whose env holds the old token) stop matching from then on.
func TestEnsureAgentToken_ReusesExistingToken(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureAgentToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureAgentToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("second call changed the token: %q → %q", first, second)
	}
	if got := ReadAgentToken(dir); got != first {
		t.Fatalf("token on disk does not match return value: %q vs %q", got, first)
	}
}

// TestEnsureAgentToken_NeverUnlinksExistingFile
// Reclaiming an empty file necessarily requires unlink, but "if it is still empty after waiting a few milliseconds, the writer is dead" does not hold:
// The writer may just have been descheduled, or a filesystem write may have stalled briefly. In that case unlink would detach the inode of a writer that is still alive,
// which would still write A into the nameless inode and return A,
// while the disk keeps B published by the reclaimer - the container gets A, the Manager reads B, and it returns 401 forever.
// So the hard requirement here is: EnsureAgentToken must never delete a file that already holds the formal name.
func TestEnsureAgentToken_NeverUnlinksExistingFile(t *testing.T) {
	cases := map[string]string{
		"零字节（早先版本写入失败的残留）": "",
		"只有空白（读出来也是空串）":    "   \n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, AgentTokenFile)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}

			if _, err := EnsureAgentToken(dir); err == nil {
				t.Fatal("a file that holds the name but whose content cannot be read should be an error, not silently taken over")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("original file must not be deleted: %v", err)
			}
			if string(got) != content {
				t.Fatalf("original file content was changed: %q → %q", content, string(got))
			}
		})
	}
}

// TestEnsureAgentToken_LeavesNoTempFileBehind
// The temp file is an implementation detail; neither the success nor the failure path should leave it behind in the Runner directory.
func TestEnsureAgentToken_LeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureAgentToken(dir); err != nil {
		t.Fatal(err)
	}
	// Go through the "name already taken" branch once more
	if _, err := EnsureAgentToken(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != AgentTokenFile {
			t.Fatalf("temp file left behind in directory: %s", e.Name())
		}
	}
}

// TestEnsureAgentToken_PublishedFileIsNeverEmpty
// This is the point of atomic publishing: the name .agent_token either does not exist or its content is complete.
// There is no intermediate "created but not yet fully written" state, so there is no empty file to reclaim.
func TestEnsureAgentToken_PublishedFileIsNeverEmpty(t *testing.T) {
	dir := t.TempDir()
	token, err := EnsureAgentToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, AgentTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || string(b) != token {
		t.Fatalf("published file content is incomplete: %q (return value %q)", string(b), token)
	}
}

// TestEnsureAgentToken_Format pins the token's shape: 64 lowercase hexadecimal characters.
//
// The generator changed from rand.Read(32 bytes)+hex.EncodeToString to secure.RandomHex(32),
// and the two must be fully equivalent. The token is written to .agent_token and also injected into the container as AGENT_TOKEN,
// but agent_token's drift check only looks at whether the container has a non-empty value, and does not compare the value
// (see the note in drift.go) - changing the encoding will not trigger a rebuild, it will only leave existing containers
// returning 401 from then on, with no visible reason in the UI.
func TestEnsureAgentToken_Format(t *testing.T) {
	token, err := EnsureAgentToken(t.TempDir())
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	if len(token) != agentTokenBytes*2 {
		t.Fatalf("token length should be %d (hex of %d bytes), got %d: %q",
			agentTokenBytes*2, agentTokenBytes, len(token), token)
	}
	isLowerHex := func(r rune) bool {
		return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
	}
	for _, r := range token {
		if !isLowerHex(r) {
			t.Fatalf("token should be lowercase hexadecimal, found %q: %s", r, token)
		}
	}
}
