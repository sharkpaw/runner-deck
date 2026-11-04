package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

func findCheck(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("self-check item %q not found, actual: %+v", name, results)
	return CheckResult{}
}

func TestCheckBasePath_WritableDir(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	got := checkBasePath(cfg)
	if got.Level != CheckOK {
		t.Fatalf("writable directory should pass: %+v", got)
	}
	// Probe files must be fully cleaned up, or they will leak into the runner list
	entries, err := os.ReadDir(cfg.Runners.BasePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("self-check should not leave files behind: %v", entries)
	}
}

func TestCheckBasePath_DoesNotTouchExistingFile(t *testing.T) {
	// A fixed filename would make the self-check open and delete a user file with the same name; probes must use a unique name
	dir := t.TempDir()
	victim := filepath.Join(dir, ".preflight-write-test")
	if err := os.WriteFile(victim, []byte("用户数据"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Runners.BasePath = dir
	if got := checkBasePath(cfg); got.Level != CheckOK {
		t.Fatalf("writable directory should pass: %+v", got)
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("same-named file was deleted by the self-check: %v", err)
	}
	if string(data) != "用户数据" {
		t.Errorf("same-named file content was overwritten: %q", data)
	}
}

func TestCheckBasePath_Missing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.BasePath = filepath.Join(t.TempDir(), "not-created")
	got := checkBasePath(cfg)
	if got.Level != CheckError {
		t.Fatalf("missing directory should report error: %+v", got)
	}
	if !strings.Contains(got.Hint, "mkdir -p") {
		t.Errorf("should give an actionable suggestion: %q", got.Hint)
	}
}

func TestCheckBasePath_NotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Runners.BasePath = f
	if got := checkBasePath(cfg); got.Level != CheckError {
		t.Fatalf("regular file should report error: %+v", got)
	}
}

func TestCheckBasePath_Unwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permission bits, so an unwritable case cannot be constructed")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0500); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Runners.BasePath = dir
	got := checkBasePath(cfg)
	if got.Level != CheckError || !strings.Contains(got.Hint, "chown") {
		t.Fatalf("unwritable directory should report error and suggest chown: %+v", got)
	}
}

func TestCheckDefaultModeDocker_DinDReachable(t *testing.T) {
	// Start a real listening port, and report ok only once it is confirmed reachable
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	t.Setenv("DOCKER_HOST", "tcp://"+ln.Addr().String())
	got := checkDefaultModeDocker(context.Background())
	if got.Level != CheckOK || !strings.Contains(got.Message, "reachable") {
		t.Fatalf("DinD should be ok when reachable: %+v", got)
	}
}

func TestCheckDefaultModeDocker_DinDUnreachable(t *testing.T) {
	// Reporting ok based only on the tcp:// prefix would show a false green when DinD is not running
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // Close it immediately so the port is guaranteed unreachable

	t.Setenv("DOCKER_HOST", "tcp://"+addr)
	got := checkDefaultModeDocker(context.Background())
	if got.Level != CheckWarn {
		t.Fatalf("must not report ok when DinD is unreachable: %+v", got)
	}
}

func TestTCPAddr(t *testing.T) {
	if got := tcpAddr("runner-dind"); got != "runner-dind:2375" {
		t.Errorf("default port should be filled in as 2375: %q", got)
	}
	if got := tcpAddr("runner-dind:2376"); got != "runner-dind:2376" {
		t.Errorf("an existing port should not be changed: %q", got)
	}
}

func TestCheckDefaultModeDocker_MissingSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "absent.sock"))
	got := checkDefaultModeDocker(context.Background())
	if got.Level != CheckWarn {
		t.Fatalf("missing socket should warn: %+v", got)
	}
}

func TestPreflight_DefaultModeSkipsContainerChecks(t *testing.T) {
	// In default mode, networks and images should not be inspected, to avoid flooding hosts without docker with unrelated errors
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	cfg.Runners.ContainerMode = false
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	t.Setenv("DOCKER_HOST", "tcp://"+ln.Addr().String())

	results := Preflight(context.Background(), cfg)
	// Assert by name rather than by count: adding an unrelated check should not fail this case,
	// What it pins down is 'default mode does not touch containers'
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
		if r.Name == "container network" || r.Name == "runner image" {
			t.Errorf("default mode should not run container-related checks: %+v", r)
		}
	}
	want := []string{"runners directory", "runner directory permissions", "runner isolation", "Docker in jobs"}
	for _, w := range want {
		findCheck(t, results, w)
	}
	if len(results) != len(want) {
		t.Fatalf("default mode checks should be %v, actual %v", want, names)
	}
}

// TestCheckDefaultModeIsolation_WarnsOnlyWithMoreThanOneRunner pins the threshold of this check.
//
// The tier boundary is whether another runner can be read: none with 0 or 1, present from 2 on.
// If the condition is written as n < 2, or this check is simply removed, the three subtests below all turn red together:
// The former makes 0/1 warn while 2 reports ok; the latter makes findCheck fail to find this item.
func TestCheckDefaultModeIsolation_WarnsOnlyWithMoreThanOneRunner(t *testing.T) {
	item := func(n int) []config.RunnerItem {
		items := make([]config.RunnerItem, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, config.RunnerItem{Name: fmt.Sprintf("r%d", i)})
		}
		return items
	}
	for _, tc := range []struct {
		n    int
		want CheckLevel
	}{
		{0, CheckOK},
		{1, CheckOK},
		{2, CheckWarn},
		{3, CheckWarn},
	} {
		cfg := &config.Config{}
		cfg.Runners.Items = item(tc.n)
		got := checkDefaultModeIsolation(cfg)
		if got.Level != tc.want {
			t.Errorf("%d runners have level %v, should be %v (%+v)", tc.n, got.Level, tc.want, got)
		}
		if got.Name != "runner isolation" {
			t.Errorf("the check name for %d runners is %q", tc.n, got.Name)
		}
	}
}

// TestCheckDefaultModeIsolation_WarningNamesTheCount: the warning must include the actual entry count:
// 'How many runners share this one container' is the only basis readers have for judging severity,
// Without it, the message degrades into a notice that gives no basis for judgment.
func TestCheckDefaultModeIsolation_WarningNamesTheCount(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.Items = []config.RunnerItem{{Name: "a"}, {Name: "b"}}
	got := checkDefaultModeIsolation(cfg)
	if got.Level != CheckWarn {
		t.Fatalf("2 runners should report warn: %+v", got)
	}
	if !strings.Contains(got.Message, "2") {
		t.Errorf("message does not contain entry count 2: %q", got.Message)
	}
	// The hint must let the reader directly follow it to change the config, otherwise readers know there is a problem but not what to do next
	if !strings.Contains(got.Hint, "container_mode") {
		t.Errorf("hint did not give container_mode: %q", got.Hint)
	}
}

// TestPreflight_DefaultModeWithTwoRunnersWarnsAboutIsolation checks this end to end
// since it sits on the default-mode branch. Unit tests call the function directly and can't tell whether it is wired into Preflight.
func TestPreflight_DefaultModeWithTwoRunnersWarnsAboutIsolation(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	cfg.Runners.ContainerMode = false
	cfg.Runners.Items = []config.RunnerItem{{Name: "a"}, {Name: "b"}}
	got := findCheck(t, Preflight(context.Background(), cfg), "runner isolation")
	if got.Level != CheckWarn {
		t.Fatalf("default mode with 2 runners should report warn: %+v", got)
	}
}

// TestPreflight_ContainerModeHasNoIsolationCheck: in container mode each runner gets its own container,
// so this check makes no sense there. Reporting it would only suggest container mode has the same problem.
func TestPreflight_ContainerModeSkipsTheIsolationCheck(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	cfg.Runners.ContainerMode = true
	cfg.Runners.Items = []config.RunnerItem{{Name: "a"}, {Name: "b"}}
	for _, r := range Preflight(context.Background(), cfg) {
		if r.Name == "runner isolation" {
			t.Errorf("container mode should have no runner isolation check: %+v", r)
		}
	}
}

func TestPreflight_NilConfig(t *testing.T) {
	got := Preflight(context.Background(), nil)
	if len(got) != 1 || got[0].Level != CheckError {
		t.Fatalf("empty config should return a single error: %+v", got)
	}
}

func TestCheckJobDockerBackend_None(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.JobDockerBackend = "none"
	if got := checkJobDockerBackend(context.Background(), cfg); got.Level != CheckOK {
		t.Fatalf("none backend has no prerequisites and should pass: %+v", got)
	}
}

func TestInGroup(t *testing.T) {
	if !inGroup(os.Getgid()) {
		t.Error("current process should belong to its own primary group")
	}
	if inGroup(-1) {
		t.Error("a nonexistent GID should not be judged as a member")
	}
}

func TestLogPreflight_FormatsByLevel(t *testing.T) {
	var lines []string
	orig := preflightLogf
	preflightLogf = func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(strings.ReplaceAll(format, "%s", "")+joinAny(args)))
	}
	defer func() { preflightLogf = orig }()

	LogPreflight([]CheckResult{
		ok("A", "正常"),
		warn("B", "有点问题", "建议 B"),
		fail("C", "挂了", "建议 C"),
	})
	if len(lines) != 3 {
		t.Fatalf("should output 3 lines: %v", lines)
	}
	if !strings.Contains(lines[0], "✓") || strings.Contains(lines[0], "建议") {
		t.Errorf("ok item should not carry a suggestion: %q", lines[0])
	}
	if !strings.Contains(lines[1], "!") || !strings.Contains(lines[1], "建议 B") {
		t.Errorf("warn item should carry a suggestion: %q", lines[1])
	}
	if !strings.Contains(lines[2], "✗") || !strings.Contains(lines[2], "建议 C") {
		t.Errorf("error item should carry a suggestion: %q", lines[2])
	}
}

// The first troubleshooting step in all six languages is `docker compose logs runner-manager | grep '\[preflight'`.
// That keyword is the line-start marker here, and the marker is a plain string constant. Changing it raises no error,
// and the first troubleshooting step would silently find no lines. This test ensures every level carries the marker;
// internal/docsconsistency separately ensures the keyword written in the docs matches this constant.
func TestLogPreflight_EveryLineCarriesTheDocumentedMarker(t *testing.T) {
	var lines []string
	orig := preflightLogf
	preflightLogf = func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(strings.ReplaceAll(format, "%s", "")+joinAny(args)))
	}
	defer func() { preflightLogf = orig }()

	LogPreflight([]CheckResult{
		ok("A", "正常"),
		warn("B", "有点问题", "建议 B"),
		fail("C", "挂了", "建议 C"),
	})
	for _, l := range lines {
		if !strings.HasPrefix(l, PreflightLogMarker) {
			t.Errorf("self-check log does not start with %q, so the grep in the docs won't find it: %q", PreflightLogMarker, l)
		}
	}
	// The marker must be language-neutral: mixing in Chinese characters would bring back the grep failures in the five non-Chinese docs.
	for _, r := range PreflightLogMarker {
		if r > 0x7f {
			t.Fatalf("marker %q contains non-ASCII character %q; it must be typeable on any keyboard", PreflightLogMarker, r)
		}
	}
}

func joinAny(args []any) string {
	var b strings.Builder
	for _, a := range args {
		if s, ok := a.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestRunnerImages_DedupAndPerRunnerOverride(t *testing.T) {
	restore := func() {
		_ = os.Unsetenv("FLEET_IMAGE_TAG")
		_ = os.Unsetenv("MANAGER_IMAGE")
	}
	t.Setenv("FLEET_IMAGE_TAG", "")
	t.Setenv("MANAGER_IMAGE", "")
	defer restore()

	cfg := &config.Config{}
	cfg.Runners.ContainerMode = true
	cfg.Runners.ContainerImage = "reg/global:1"
	cfg.Runners.Items = []config.RunnerItem{
		{Name: "a", ContainerImage: "reg/custom:1"},
		{Name: "b"}, // fall back to global
		{Name: "c", ContainerImage: "reg/custom:1"}, // same as a, should be deduplicated
		{Name: "d", ContainerImage: "reg/other:2"},
	}
	got := RunnerImages(cfg)
	want := []string{"reg/global:1", "reg/custom:1", "reg/other:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("RunnerImages = %v, want %v", got, want)
	}
}

func TestRunnerImages_FallsBackToDefault(t *testing.T) {
	// when neither global nor items configure a mirror, still check the default mirror actually used
	cfg := &config.Config{}
	cfg.Runners.ContainerMode = true
	got := RunnerImages(cfg)
	if len(got) != 1 || got[0] != config.DefaultRunnerContainerImage() {
		t.Errorf("RunnerImages = %v, want [%s]", got, config.DefaultRunnerContainerImage())
	}
}

func TestRunnerImages_NilConfig(t *testing.T) {
	if got := RunnerImages(nil); got != nil {
		t.Errorf("nil config should return nil, got %v", got)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine([]byte("first\nsecond\nthird"), nil); got != "first" {
		t.Errorf("firstLine = %q, want %q", got, "first")
	}
	if got := firstLine([]byte("  only  \n"), nil); got != "only" {
		t.Errorf("firstLine should trim surrounding whitespace: %q", got)
	}
	// Fall back to the error text when output is empty, otherwise the self-check result would be an uninformative sentence
	if got := firstLine(nil, errors.New("exit status 125")); got != "exit status 125" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine(nil, nil); got != "" {
		t.Errorf("when both are empty, an empty string should be returned: %q", got)
	}
}

func TestRequiredRunnerTools_CoversTheKnownFailureModes(t *testing.T) {
	// These two were hit in practice: without git, checkout silently degrades; without unzip, setup-gradle fails midway
	for _, want := range []string{"git", "unzip"} {
		found := false
		for _, tool := range requiredRunnerTools {
			if tool == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("requiredRunnerTools should include %q", want)
		}
	}
}

func TestParseMissingTools(t *testing.T) {
	const marker = missingToolMarker
	// Real warning docker prints when running an amd64 image on an arm64 host; the exit code is still 0.
	// dockerCmd uses CombinedOutput, which mixes this into the probe output.
	platformWarning := "WARNING: The requested image's platform (linux/amd64) does not match " +
		"the detected host platform (linux/arm64/v8) and no specific platform was requested\n"

	cases := []struct {
		name string
		out  string
		want []string
	}{
		{"全部具备", "", nil},
		{"仅告警，不应误报", platformWarning, nil},
		{"告警与缺失混合", platformWarning + marker + "git\n" + marker + "unzip\n", []string{"git", "unzip"}},
		{"缺失项前后有空白", "  " + marker + "git  \n", []string{"git"}},
		{"未知名字不予采信", marker + "definitely-not-a-required-tool\n", nil},
		{"无标记的裸行忽略", "git\nunzip\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMissingTools([]byte(tc.out))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("parseMissingTools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseMissingTools_RecognisesEveryRequiredTool(t *testing.T) {
	// The probe script is generated from requiredRunnerTools, so the parser must recognize every one of them,
	// otherwise adding a command to the list later would yield a false green that is checked but never reported
	var out strings.Builder
	for _, tool := range requiredRunnerTools {
		out.WriteString(missingToolMarker + tool + "\n")
	}
	got := parseMissingTools([]byte(out.String()))
	if len(got) != len(requiredRunnerTools) {
		t.Fatalf("parsed %v, expected all of %v", got, requiredRunnerTools)
	}
}

// A crash in the self-check should not take Manager's startup down with it.
//
// Before switching to preflight-kit, Preflight called each check directly in sequence with no fallback at all --
// and checkRunnerImages / checkJobDockerBackend both spawn subprocesses to query docker,
// which is exactly where surprises most often happen. One panic means Manager cannot start,
// when all it was meant to do was report a few more environment details at startup.
func TestRunCheck_PanicBecomesAnErrorResultInsteadOfCrashing(t *testing.T) {
	got := runCheck(context.Background(), "会炸的检查", func(context.Context) CheckResult {
		panic("docker 客户端里的某个 nil")
	})

	if got.Level != CheckError {
		t.Fatalf("Level = %q, expected %q", got.Level, CheckError)
	}
	if got.Name != "会炸的检查" {
		t.Errorf("Name = %q; the fallback result should carry the check name, otherwise the log cannot show which check blew up", got.Name)
	}
	if !strings.Contains(got.Message, "docker 客户端里的某个 nil") {
		t.Errorf("Message should keep the panic content, got %q", got.Message)
	}
}

// Checks that return normally are unaffected by the fallback; their results pass through unchanged.
func TestRunCheck_PassesThroughNormalResults(t *testing.T) {
	for _, want := range []CheckResult{
		ok("甲", "一切正常"),
		warn("乙", "有点问题", "照这个改"),
		fail("丙", "跑不起来", "照那个改"),
	} {
		got := runCheck(context.Background(), "名字用不上", func(context.Context) CheckResult { return want })
		if got != want {
			t.Errorf("result was modified: %+v -> %+v", want, got)
		}
	}
}

// Preflight as a whole also needs a fallback: a test case cannot inject a panic into the real checks,
// so here we step back -- confirm that every item actually goes through runCheck, i.e. no path bypasses it.
func TestPreflight_EveryCheckGoesThroughRunCheck(t *testing.T) {
	src, err := os.ReadFile("preflight.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func Preflight(")
	if start < 0 {
		t.Fatal("Preflight not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("Preflight end not found")
	}
	fn := body[start : start+end]

	// Every check* call in the function body must be wrapped in runCheck.
	// checkRunnerImages is the exception: it calls runCheck for each item internally (two items per image).
	for _, name := range []string{
		"checkBasePath", "checkRunnerDirPermissions", "checkDefaultModeIsolation",
		"checkDefaultModeDocker", "checkDockerReachable", "checkNetwork", "checkNetworkMembers",
		"checkJobDockerBackend",
	} {
		if !strings.Contains(fn, name) {
			t.Errorf("%s is missing from Preflight; the test case needs updating", name)
		}
	}
	if strings.Count(fn, "runCheck(") < 8 {
		t.Errorf("runCheck call count %d in Preflight is less than the number of checks; some path may bypass the panic fallback:\n%s",
			strings.Count(fn, "runCheck("), fn)
	}
}

// ---- container network members ----

// TestUnexpectedNetworkMembers pure function part: which members count as belonging to this deployment, and which don't.
//
// Each case gives self separately, because 'hostname unavailable' is one of the behaviors to pin down,
// and it is exactly the one most easily written wrong as 'always green'.
func TestUnexpectedNetworkMembers(t *testing.T) {
	const self = "1a2b3c4d5e6f" // Manager container hostname: when Docker does not specify one, it is the 12-character short ID
	selfID := self + strings.Repeat("0", 52)
	expected := map[string]bool{"runner-dind": true, "runner-a": true}

	tests := []struct {
		name    string
		self    string
		members map[string]string
		want    []string
	}{
		{
			name: "自身、DinD 与 Runner 都在预期内",
			self: self,
			members: map[string]string{
				selfID:     "runner-manager",
				"c0ffee11": "runner-dind",
				"c0ffee22": "runner-a",
			},
		},
		{
			name: "多出一个无关容器",
			self: self,
			members: map[string]string{
				selfID:     "runner-manager",
				"c0ffee11": "runner-dind",
				"c0ffee22": "runner-a",
				"c0ffee33": "grafana",
			},
			want: []string{"grafana"},
		},
		{
			name:    "hostname 只是容器 ID 的前缀，仍算自身",
			self:    self,
			members: map[string]string{selfID: "runner-manager"},
		},
		{
			name: "结果按名字排序",
			self: self,
			members: map[string]string{
				"c0ffee44": "zoo",
				"c0ffee55": "alpha",
				"c0ffee66": "mid",
			},
			want: []string{"alpha", "mid", "zoo"},
		},
		{
			// An empty prefix would make HasPrefix always true, counting the whole network as self;
			// then this check would silently never report anything
			name:    "取不到 hostname 时不把整张网络算成自身",
			self:    "",
			members: map[string]string{selfID: "runner-manager", "c0ffee77": "grafana"},
			want:    []string{"grafana", "runner-manager"},
		},
		{
			name:    "没有名字的端点用短 ID 点名",
			self:    self,
			members: map[string]string{"abcdef0123456789": ""},
			want:    []string{"abcdef012345"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Map iteration order differs each time, so running several passes is needed to show the result is truly stable
			for i := 0; i < 20; i++ {
				got := unexpectedNetworkMembers(tc.members, tc.self, expected)
				if strings.Join(got, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("run %d got %v, want %v", i+1, got, tc.want)
				}
			}
		})
	}
}

// TestListNetworkMembers collapses to a count beyond ten, otherwise one log line would be flooded by a single network's member list
func TestListNetworkMembers(t *testing.T) {
	var many []string
	for i := 0; i < maxListedNetworkMembers+3; i++ {
		many = append(many, fmt.Sprintf("c%d", i))
	}

	if got, want := listNetworkMembers([]string{"a", "b"}), "a, b"; got != want {
		t.Errorf("under the limit, the list should be printed as-is: got %q, want %q", got, want)
	}
	got := listNetworkMembers(many)
	if !strings.HasSuffix(got, "…and 3 more") {
		t.Errorf("the overflow should collapse into a count, got %q", got)
	}
	if strings.Count(got, ", ") != maxListedNetworkMembers {
		t.Errorf("should name only %d entries, got %q", maxListedNetworkMembers, got)
	}
}

// TestAnyDindBackend: items[].job_docker_backend can override each item individually, so the global value proves nothing in either direction
func TestAnyDindBackend(t *testing.T) {
	tests := []struct {
		name   string
		global string
		items  []config.RunnerItem
		want   bool
	}{
		{name: "还没配 Runner 时按全局值算", global: "dind", want: true},
		{name: "还没配 Runner 且全局是 none", global: "none", want: false},
		{name: "还没配 Runner 且全局留空，按默认的 dind 算", global: "", want: true},
		{
			name:   "全局 none，但某个 Runner 单独开了 dind",
			global: "none",
			items:  []config.RunnerItem{{Name: "a"}, {Name: "b", JobDockerBackend: "dind"}},
			want:   true,
		},
		{
			name:   "全局 dind，但每个 Runner 都覆盖成了别的",
			global: "dind",
			items:  []config.RunnerItem{{Name: "a", JobDockerBackend: "none"}, {Name: "b", JobDockerBackend: "host-socket"}},
			want:   false,
		},
		{
			name:   "Runner 没覆盖时回落全局",
			global: "dind",
			items:  []config.RunnerItem{{Name: "a"}},
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Runners.JobDockerBackend = tc.global
			cfg.Runners.Items = tc.items
			if got := anyDindBackend(cfg); got != tc.want {
				t.Fatalf("anyDindBackend = %v, want %v", got, tc.want)
			}
		})
	}
}

// networkMembersConfig: a config with container mode, the dind backend, and runner-a registered
func networkMembersConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{Runners: config.RunnersConfig{
		BasePath:         t.TempDir(),
		ContainerMode:    true,
		ContainerImage:   "img:tag",
		ContainerNetwork: "runner-net",
		JobDockerBackend: "dind",
		DindHost:         "runner-dind",
		AgentPort:        8081,
		// Container name is not the Runner name: ContainerName("a") is github-runner-a,
		// The expected member table must be compared by container name, otherwise this deployment's own Runner would be reported as an unexpected member
		Items: []config.RunnerItem{{Name: "a", TargetType: "repo", Target: "o/r"}},
	}}
}

// fakeNetworkInspect makes the fake docker return the given .Containers for network inspect
func fakeNetworkInspect(t *testing.T, containersJSON string) string {
	t.Helper()
	return fakeDockerProgram(t, "case \"$1\" in\n"+
		"  network)\n"+
		"    cat <<'JSON'\n"+containersJSON+"\nJSON\n"+
		"    ;;\n"+
		"esac\n"+
		"exit 0\n")
}

// TestCheckNetworkMembers_WarnsAboutContainersOutsideTheDeployment
// Extra containers on the network must be named: they get an unauthenticated privileged daemon.
func TestCheckNetworkMembers_WarnsAboutContainersOutsideTheDeployment(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// Manager's own entry: the full container ID is 64 characters, and the hostname is its prefix
	logPath := fakeNetworkInspect(t, fmt.Sprintf(`{
  %q: {"Name": "runner-manager"},
  "c0ffee11deadbeef": {"Name": "runner-dind"},
  "c0ffee22deadbeef": {"Name": "github-runner-a"},
  "c0ffee33deadbeef": {"Name": "grafana"},
  "c0ffee44deadbeef": {"Name": "prometheus"}
}`, host+strings.Repeat("0", 52)))

	got := checkNetworkMembers(context.Background(), networkMembersConfig(t))
	if got.Level != CheckWarn {
		t.Fatalf("should warn when unrelated containers are on the network: %+v", got)
	}
	if got.Name != "container network members" {
		t.Errorf("Name = %q", got.Name)
	}
	for _, want := range []string{"2 containers on runner-net", "grafana", "prometheus"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message should contain %q, got %q", want, got.Message)
		}
	}
	// This deployment's own three containers are reported as "not belonging to this deployment", so no one will look at this check twice
	for _, unwanted := range []string{"runner-manager", "runner-dind", "github-runner-a"} {
		if strings.Contains(got.Message, unwanted) {
			t.Errorf("The message should not contain %q (that is this deployment's own container), got %q", unwanted, got.Message)
		}
	}
	if !strings.Contains(got.Hint, "docker network disconnect runner-net") {
		t.Errorf("The hint should give a command the user can follow, got %q", got.Hint)
	}
	if calls := readCalls(t, logPath); !strings.Contains(calls, "network inspect runner-net --format {{json .Containers}}") {
		t.Errorf("Should only take the .Containers segment, but actually executed: %s", calls)
	}
}

// TestCheckNetworkMembers_OnlyTheDeploymentsOwnContainers reports ok when the network has only this deployment's containers
func TestCheckNetworkMembers_OnlyTheDeploymentsOwnContainers(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	fakeNetworkInspect(t, fmt.Sprintf(`{
  %q: {"Name": "runner-manager"},
  "c0ffee11deadbeef": {"Name": "runner-dind"},
  "c0ffee22deadbeef": {"Name": "github-runner-a"}
}`, host+strings.Repeat("0", 52)))

	got := checkNetworkMembers(context.Background(), networkMembersConfig(t))
	if got.Level != CheckOK {
		t.Fatalf("When the network has only this deployment's containers it should be ok: %+v", got)
	}
	if want := "the network runner-net has no unexpected members"; got.Message != want {
		t.Errorf("Message = %q, expected %q", got.Message, want)
	}
}

// TestCheckNetworkMembers_UnreadableInspectIsSkippedNotReported
// When the member table can't be read, report ok: cases like "network does not exist" are already reported as fail by checkNetwork,
// reporting it again here would say the same thing twice, in a more roundabout way.
func TestCheckNetworkMembers_UnreadableInspectIsSkippedNotReported(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "inspect 失败", body: "echo 'Error: No such network: runner-net' >&2\nexit 1\n"},
		{name: "输出不是 JSON", body: "echo 'not json at all'\nexit 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeDockerProgram(t, tc.body)
			got := checkNetworkMembers(context.Background(), networkMembersConfig(t))
			if got.Level != CheckOK {
				t.Fatalf("When the member table can't be read it should skip rather than error: %+v", got)
			}
			if !strings.Contains(got.Message, "skipped") {
				t.Errorf("The message should say this item was skipped, got %q", got.Message)
			}
		})
	}
}

// TestPreflight_NetworkMembersOnlyWhenADindBackendIsInUse
// When no Runner uses dind, the unauthenticated daemon does not exist on the network, so this item should not appear in the results.
func TestPreflight_NetworkMembersOnlyWhenADindBackendIsInUse(t *testing.T) {
	const checkName = "container network members"

	t.Run("dind 时有这一项", func(t *testing.T) {
		fakeNetworkInspect(t, `{}`)
		findCheck(t, Preflight(context.Background(), networkMembersConfig(t)), checkName)
	})

	t.Run("后端是 none 时没有这一项", func(t *testing.T) {
		fakeNetworkInspect(t, `{"c0ffee33deadbeef": {"Name": "grafana"}}`)
		cfg := networkMembersConfig(t)
		cfg.Runners.JobDockerBackend = "none"
		for _, r := range Preflight(context.Background(), cfg) {
			if r.Name == checkName {
				t.Fatalf("When no Runner uses dind, this item should not be executed: %+v", r)
			}
		}
	})
}
