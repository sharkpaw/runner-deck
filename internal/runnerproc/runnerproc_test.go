package runnerproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// fakeProc builds a fake procfs: pid -> argv, used by classification test cases, independent of real processes
func fakeProc(t *testing.T, procs map[int][]string) {
	t.Helper()
	root := t.TempDir()
	for pid, argv := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create fake proc directory: %v", err)
		}
		var buf []byte
		for _, a := range argv {
			buf = append(buf, []byte(a)...)
			buf = append(buf, 0)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), buf, 0644); err != nil {
			t.Fatalf("failed to write fake cmdline: %v", err)
		}
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

func TestFind_ClaimsListenerAndSupervisors(t *testing.T) {
	const dir = "/runner"
	fakeProc(t, map[int][]string{
		// run.sh: the kernel follows the shebang, the interpreter is in argv[0], the script in argv[1]
		11: {"/bin/bash", dir + "/run.sh"},
		// run-helper.sh, which run.sh copies out and invokes
		12: {"/bin/bash", dir + "/run-helper.sh"},
		// The real listener, launched by run-helper.sh via an absolute path
		13: {dir + "/bin/Runner.Listener", "run"},
		// Unrelated process
		14: {"/usr/sbin/sshd", "-D"},
	})
	got := Find(dir)
	want := []int{11, 12, 13}
	if len(got) != len(want) {
		t.Fatalf("Find returned %v, expected %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Find returned %v, expected %v", got, want)
		}
	}
	if !Running(dir) {
		t.Fatal("Running should be true")
	}
}

// The watchdog script must come before the listener: Stop sends signals in this order,
// so run.sh is stopped first and its while loop does not relaunch a listener
func TestFind_SupervisorsSortBeforeListener(t *testing.T) {
	const dir = "/runner"
	fakeProc(t, map[int][]string{
		// Deliberately give the listener a smaller pid than the watchdog script
		7:  {dir + "/bin/Runner.Listener", "run"},
		90: {"/bin/bash", dir + "/run.sh"},
	})
	got := Find(dir)
	if len(got) != 2 || got[0] != 90 || got[1] != 7 {
		t.Fatalf("Find returned %v, expected watchdog script 90 before listener 7", got)
	}
}

// Only the listener running, with no watchdog script, still counts as running (e.g. run.sh was replaced or Runner.Listener is run directly)
func TestFind_ListenerAloneCounts(t *testing.T) {
	const dir = "/runner"
	fakeProc(t, map[int][]string{
		21: {dir + "/bin/Runner.Listener", "run"},
	})
	if !Running(dir) {
		t.Fatal("Should be judged running when only Runner.Listener is running")
	}
}

// Only the watchdog script running with the listener absent also counts as running: run-helper.sh handles exit code 2 by
// sleeping 5 seconds, returning, and run.sh relaunching it, so those 5 seconds must not be judged dead
func TestFind_SupervisorAloneCountsDuringListenerRestart(t *testing.T) {
	const dir = "/runner"
	fakeProc(t, map[int][]string{
		22: {"/bin/bash", dir + "/run.sh"},
	})
	if !Running(dir) {
		t.Fatal("run.sh still running during the listener restart gap should be judged running")
	}
}

// Multiple runners on the same machine each claim their own directory and must not cross over
func TestFind_DoesNotClaimAnotherInstallDir(t *testing.T) {
	fakeProc(t, map[int][]string{
		31: {"/bin/bash", "/runners/droiddesk-2/run.sh"},
		32: {"/runners/droiddesk-2/bin/Runner.Listener", "run"},
	})
	if Running("/runners/droiddesk-3") {
		t.Fatal("droiddesk-3 has no process and must not be claimed by droiddesk-2's process")
	}
	if !Running("/runners/droiddesk-2") {
		t.Fatal("droiddesk-2 should be judged running")
	}
}

// A script path appearing in a command line does not mean the runner is running
func TestFind_IgnoresUnrelatedCommandMentioningTheScript(t *testing.T) {
	const dir = "/runner"
	fakeProc(t, map[int][]string{
		41: {"/usr/bin/grep", dir + "/run.sh"},
		42: {"/usr/bin/tail", "-f", dir + "/run.sh"},
		43: {"/usr/bin/vim", dir + "/bin/Runner.Listener"},
	})
	if Running(dir) {
		t.Fatalf("grep/tail/vim mentioning the script path should not count as the runner running, Find=%v", Find(dir))
	}
}

// Kernel threads have an empty cmdline; this must not cause a panic or a false positive
func TestFind_SkipsEmptyCmdline(t *testing.T) {
	const dir = "/runner"
	fakeProc(t, map[int][]string{
		2: {},
		3: {},
	})
	if Running(dir) {
		t.Fatal("An empty cmdline should not count as the runner running")
	}
}

// Without /proc (Windows, or procfs not mounted), report not found rather than guessing
func TestFind_NoProcfsReportsNotRunning(t *testing.T) {
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { procRoot = old })
	if Running("/runner") {
		t.Fatal("Should report not running when there is no procfs")
	}
	if got := Find("/runner"); got != nil {
		t.Fatalf("Find should return nil without procfs, got %v", got)
	}
}

func TestFind_EmptyInstallDir(t *testing.T) {
	if Running("") {
		t.Fatal("An empty install directory should not be judged running")
	}
}

// This is why this package exists: actions/runner does not write a pid file, and .path holds a PATH string.
// The check must look only at processes; no files in the directory may influence the result.
func TestRunning_IgnoresPidLookalikeFiles(t *testing.T) {
	dir := t.TempDir()
	// A real, live pid; the old implementation would have counted it as the runner running
	if err := os.WriteFile(filepath.Join(dir, "Runner.Listener.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".path"),
		[]byte("/usr/local/bin:/usr/bin:/bin"), 0644); err != nil {
		t.Fatal(err)
	}
	fakeProc(t, map[int][]string{51: {"/usr/sbin/sshd", "-D"}})
	if Running(dir) {
		t.Fatal("A pid file in the directory should not affect the result; only the process decides")
	}
}

// End to end: verify against the real /proc and real processes; a fake procfs cannot prove this step
func TestFind_RealProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Process detection depends on /proc and is verified only on Linux")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("No usable /proc in the current environment")
	}
	dir := t.TempDir()

	// Watchdog script: copy the shebang form of actions/runner, do not exec,
	// otherwise bash gets replaced and argv is no longer `/bin/bash <dir>/run.sh`.
	//
	// Nor should `sleep 120` be used to keep it hanging: bash forks a child process to exec sleep,
	// Between fork and execve, the child's cmdline is still bash's argv, so it gets mistaken for
	// a second watchdog script, so the pid-order assertions below fail randomly. The window is only a few microseconds,
	// rarely hit locally, but on CI it bites whenever the machine is busy.
	// read is a builtin and the redirection is done by the current shell, so opening a FIFO with no writer blocks in place,
	// without forking at any point.
	script := filepath.Join(dir, "run.sh")
	fifo := filepath.Join(dir, ".test-block")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Failed to create FIFO: %v", err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/bash\nread -r -t 120 < \""+fifo+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Listener: it needs a real executable so that argv[0] is its own absolute path;
	// a shebang script's argv[0] is the interpreter, so it can't be simulated
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	// Build a small sleeper instead of copying /bin/sleep: on systems where coreutils
	// is a multicall binary (Ubuntu's uutils), a copy under another name exits at once.
	listener := ListenerBinary(dir)
	src := filepath.Join(t.TempDir(), "main.go")
	code := "package main\nimport (\"os\"; \"strconv\"; \"time\")\n" +
		"func main() { n, _ := strconv.Atoi(os.Args[1]); time.Sleep(time.Duration(n) * time.Second) }\n"
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", listener, src).CombinedOutput(); err != nil {
		t.Skipf("cannot build the listener stub: %v: %s", err, out)
	}

	if Running(dir) {
		t.Fatal("No process has started yet; it should not be reported as running")
	}

	sup := exec.Command(script)
	sup.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sup.Start(); err != nil {
		t.Fatalf("Failed to start watchdog script: %v", err)
	}
	lis := exec.Command(listener, "120")
	lis.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := lis.Start(); err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-sup.Process.Pid, syscall.SIGKILL)
		_ = lis.Process.Kill()
		_, _ = sup.Process.Wait()
		_, _ = lis.Process.Wait()
	})

	if !waitFor(func() bool { return len(Find(dir)) == 2 }) {
		t.Fatalf("Real process not claimed, Find=%v (expected watchdog script %d and listener %d)",
			Find(dir), sup.Process.Pid, lis.Process.Pid)
	}
	got := Find(dir)
	if got[0] != sup.Process.Pid || got[1] != lis.Process.Pid {
		t.Fatalf("Find=%v, expected [%d %d] (watchdog script first)", got, sup.Process.Pid, lis.Process.Pid)
	}

	// Another directory's processes must not be claimed by these two
	if Running(t.TempDir()) {
		t.Fatal("Unrelated directory should not be claimed")
	}

	// After the process exits it must switch to not running, otherwise "start if registered but not running" never triggers
	_ = syscall.Kill(-sup.Process.Pid, syscall.SIGKILL)
	_ = lis.Process.Kill()
	_, _ = sup.Process.Wait()
	_, _ = lis.Process.Wait()
	if !waitFor(func() bool { return !Running(dir) }) {
		t.Fatalf("Process has exited but Running is still true, Find=%v", Find(dir))
	}
}

// waitFor polls until the condition holds, for at most 5 seconds
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// FindMany scans once and must assign each process to the right entry; it must not count a's processes toward b
func TestFindMany_PartitionsAcrossInstallDirs(t *testing.T) {
	a, b, c := "/runners/a", "/runners/b", "/runners/c"
	fakeProc(t, map[int][]string{
		61: {"/bin/bash", a + "/run.sh"},
		62: {a + "/bin/Runner.Listener", "run"},
		63: {b + "/bin/Runner.Listener", "run"},
		64: {"/usr/sbin/sshd", "-D"},
	})
	got := FindMany([]string{a, b, c})
	if len(got[a]) != 2 || got[a][0] != 61 || got[a][1] != 62 {
		t.Fatalf("a should be [61 62], got %v", got[a])
	}
	if len(got[b]) != 1 || got[b][0] != 63 {
		t.Fatalf("b should be [63], got %v", got[b])
	}
	if _, ok := got[c]; ok {
		t.Fatalf("c has no processes and should not appear in the result, got %v", got[c])
	}
}

// Directories with similar prefixes must not be mixed up: /runners/a and /runners/ab
func TestFindMany_PrefixSimilarDirsDoNotCollide(t *testing.T) {
	a, ab := "/runners/a", "/runners/ab"
	fakeProc(t, map[int][]string{
		71: {ab + "/bin/Runner.Listener", "run"},
	})
	got := FindMany([]string{a, ab})
	if _, ok := got[a]; ok {
		t.Fatalf("/runners/a should not claim /runners/ab's processes, got %v", got[a])
	}
	if len(got[ab]) != 1 || got[ab][0] != 71 {
		t.Fatalf("/runners/ab should be [71], got %v", got[ab])
	}
}

// Different spellings of the same directory must each get results
func TestFindMany_SameDirDifferentSpellings(t *testing.T) {
	fakeProc(t, map[int][]string{
		81: {"/runners/a/bin/Runner.Listener", "run"},
	})
	got := FindMany([]string{"/runners/a", "/runners/a/", "/runners/./a"})
	for _, k := range []string{"/runners/a", "/runners/a/", "/runners/./a"} {
		if len(got[k]) != 1 || got[k][0] != 81 {
			t.Fatalf("%q should be [81], got %v", k, got[k])
		}
	}
}

func TestFindMany_EmptyInput(t *testing.T) {
	fakeProc(t, map[int][]string{91: {"/runners/a/bin/Runner.Listener", "run"}})
	if got := FindMany(nil); got != nil {
		t.Fatalf("Empty input should return nil, got %v", got)
	}
	if got := FindMany([]string{"", ""}); got != nil {
		t.Fatalf("All-empty strings should return nil, got %v", got)
	}
}
