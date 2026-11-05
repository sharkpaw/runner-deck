package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

func TestParseSize(t *testing.T) {
	cases := map[string]uint64{
		"0B":      0,
		"512B":    512,
		"1kB":     1000,
		"1.5MB":   1500000,
		"2GiB":    2 << 30,
		" 10MiB ": 10 << 20,
		"--":      0,
		"":        0,
	}
	for in, want := range cases {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseStats(t *testing.T) {
	out := []byte(`{"Name":"github-runner-a","CPUPerc":"12.50%","MemUsage":"10MiB / 1GiB"}
not json
{"Name":"","CPUPerc":"1%","MemUsage":"1MiB / 1GiB"}
{"Name":"postgres","CPUPerc":"0.10%","MemUsage":"20MiB / 2GiB"}
`)
	got := parseStats(out)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Name != "github-runner-a" || got[0].CPUPct != 12.5 || got[0].MemBytes != 10<<20 || got[0].MemLimit != 1<<30 {
		t.Errorf("first row = %+v", got[0])
	}
}

func TestParseDockerDF(t *testing.T) {
	out := []byte(`{"Type":"Images","TotalCount":"3","Size":"1.2GB","Reclaimable":"100MB (8%)"}
{"Type":"Build Cache","TotalCount":7,"Size":"5GB","Reclaimable":"5GB"}
`)
	got := parseDockerDF(out)
	if len(got) != 2 || got[0].Count != "3" || got[1].Count != "7" || got[1].Size != "5GB" {
		t.Fatalf("got %+v", got)
	}
}

func TestBuildSystemRunners_SplitsRunnersFromOthers(t *testing.T) {
	list := []runner.RunnerInfo{{Name: "a", Target: "o/r"}, {Name: "b", Target: "o/r"}}
	stats := []containerStat{
		{Name: "c-a", CPUPct: 10, MemBytes: 100, MemLimit: 1000},
		{Name: "postgres", CPUPct: 1, MemBytes: 5},
		{Name: "redis", CPUPct: 2, MemBytes: 7},
	}
	sizes := map[string]uint64{"a": 42}
	rows, others := buildSystemRunners(&config.Config{}, list, stats, sizes, map[string]cgroupStats{"c-a": {OOMKills: 2, ThrottledPct: 10}}, func(n string) string { return "c-" + n })
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if !rows[0].HasStats || rows[0].CPUPct != 10 || rows[0].MemLimit != 1000 || rows[0].DiskBytes == nil || *rows[0].DiskBytes != 42 {
		t.Errorf("row a = %+v", rows[0])
	}
	if rows[0].OOMKills != 2 || rows[0].ThrottledPct != 10 {
		t.Errorf("limit warnings missing: %+v", rows[0])
	}
	if !rows[0].Running {
		t.Error("a container that docker stats lists is running")
	}
	if rows[1].HasStats || rows[1].DiskBytes != nil {
		t.Errorf("a stopped runner without a scan must report no numbers: %+v", rows[1])
	}
	if others.Count != 2 || others.CPUPct != 3 || others.MemBytes != 12 {
		t.Errorf("others = %+v", others)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"a": 10, "sub/b": 5} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := dirSize(dir); got != 15 {
		t.Errorf("dirSize = %d, want 15", got)
	}
}

func TestDiskUsage(t *testing.T) {
	d := diskUsage(t.TempDir())
	if d == nil || d.TotalBytes == 0 || d.UsedBytes+d.FreeBytes != d.TotalBytes {
		t.Fatalf("diskUsage = %+v", d)
	}
	if diskUsage(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Error("a missing path must report nil, not zeros")
	}
}

func TestResolveLang_Turkish(t *testing.T) {
	cases := []struct{ url, accept string }{
		{"/?lang=tr", ""},
		{"/", "tr-TR,tr;q=0.9,en;q=0.8"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.url, nil)
		if tc.accept != "" {
			req.Header.Set("Accept-Language", tc.accept)
		}
		c := echo.New().NewContext(req, httptest.NewRecorder())
		if got := resolveLang(c); got != "tr" {
			t.Errorf("resolveLang(%q, %q) = %q, want tr", tc.url, tc.accept, got)
		}
	}
}

func stubDocker(t *testing.T, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	orig := dockerRun
	dockerRun = func(_ context.Context, args ...string) ([]byte, error) { return fn(args...) }
	statsCache = ttlCache[[]containerStat]{}
	dockerCache = ttlCache[[]SystemDockerUsage]{}
	cgroupCache = ttlCache[map[string]cgroupStats]{}
	t.Cleanup(func() {
		dockerRun = orig
		statsCache = ttlCache[[]containerStat]{}
		dockerCache = ttlCache[[]SystemDockerUsage]{}
		cgroupCache = ttlCache[map[string]cgroupStats]{}
		cgroupCache = ttlCache[map[string]cgroupStats]{}
	})
}

func TestCollectSystem_JoinsDockerNumbers(t *testing.T) {
	calls := 0
	stubDocker(t, func(args ...string) ([]byte, error) {
		calls++
		if args[0] == "stats" {
			return []byte(`{"Name":"github-runner-a","CPUPerc":"5%","MemUsage":"1MiB / 2MiB"}` + "\n"), nil
		}
		return []byte(`{"Type":"Images","TotalCount":"1","Size":"1GB","Reclaimable":"0B"}` + "\n"), nil
	})
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	list := []runner.RunnerInfo{{Name: "a", Target: "o/r", InstallDir: cfg.Runners.BasePath}}

	got := collectSystem(cfg, list)
	if got.StatsError != "" || len(got.Runners) != 1 || !got.Runners[0].Running || got.Runners[0].CPUPct != 5 {
		t.Fatalf("got %+v", got)
	}
	if len(got.Docker) != 1 || got.Disk == nil || got.HostCPUs < 1 {
		t.Fatalf("got %+v", got)
	}
	collectSystem(cfg, list)
	if calls != 3 {
		t.Errorf("docker ran %d times, a second poll within the TTL must use the cache", calls)
	}
}

func TestCollectSystem_DockerFailureKeepsThePage(t *testing.T) {
	stubDocker(t, func(...string) ([]byte, error) { return nil, errors.New("no daemon") })
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	got := collectSystem(cfg, []runner.RunnerInfo{{Name: "a", InstallDir: cfg.Runners.BasePath}})
	if got.StatsError == "" || len(got.Runners) != 1 || got.Runners[0].HasStats || got.Disk == nil {
		t.Fatalf("got %+v", got)
	}
}

func TestRefreshDirSizes_RunsInBackground(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	// Another test may have left a scan running; it would overwrite the result.
	for i := 0; i < 500; i++ {
		dirSizesMu.Lock()
		busy := dirSizesScanning
		if !busy {
			dirSizes = map[string]uint64{}
			dirSizesAt = time.Time{}
		}
		dirSizesMu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	refreshDirSizes(map[string]string{"a": dir})
	for i := 0; i < 200; i++ {
		dirSizesMu.Lock()
		size, ok := dirSizes["a"]
		dirSizesMu.Unlock()
		if ok {
			if size != 7 {
				t.Fatalf("size = %d", size)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the background scan never finished")
}

func TestSystemInfo_ServesTheConfiguredRunners(t *testing.T) {
	stubDocker(t, func(...string) ([]byte, error) { return nil, errors.New("no daemon") })
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "runners:\n  base_path: " + dir + "\n  items:\n    - {name: a, target_type: repo, target: o/r}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = orig })

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/system", nil), rec)
	if err := SystemInfo(c); err != nil {
		t.Fatal(err)
	}
	var got SystemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Runners) != 1 || got.Runners[0].Name != "a" || got.StatsError == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestPageData_HasTranslationsAndLanguage(t *testing.T) {
	orig := I18nLoader
	I18nLoader = func(string) (map[string]string, error) { return map[string]string{"k": "v"}, nil }
	t.Cleanup(func() { I18nLoader = orig })
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/?lang=tr", nil), httptest.NewRecorder())
	data := PageData(c)
	if data["Lang"] != "tr" || data["T"].(map[string]string)["k"] != "v" {
		t.Fatalf("data = %+v", data)
	}
}

func TestCollectSystem_RequestCancelDoesNotReachDocker(t *testing.T) {
	var sawCanceled bool
	orig := dockerRun
	dockerRun = func(ctx context.Context, _ ...string) ([]byte, error) {
		sawCanceled = ctx.Err() != nil
		return nil, nil
	}
	statsCache = ttlCache[[]containerStat]{}
	dockerCache = ttlCache[[]SystemDockerUsage]{}
	cgroupCache = ttlCache[map[string]cgroupStats]{}
	t.Cleanup(func() {
		dockerRun = orig
		statsCache = ttlCache[[]containerStat]{}
		dockerCache = ttlCache[[]SystemDockerUsage]{}
		cgroupCache = ttlCache[map[string]cgroupStats]{}
		cgroupCache = ttlCache[map[string]cgroupStats]{}
	})
	cfg := &config.Config{}
	cfg.Runners.BasePath = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/system", nil).WithContext(ctx)
	path := filepath.Join(cfg.Runners.BasePath, "config.yaml")
	if err := os.WriteFile(path, []byte("runners:\n  base_path: "+cfg.Runners.BasePath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPath := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = origPath })
	if err := SystemInfo(echo.New().NewContext(req, httptest.NewRecorder())); err != nil {
		t.Fatal(err)
	}
	if sawCanceled {
		t.Fatal("a closed page must not cancel the shared docker call")
	}
}

func TestTTLCache_KeepsLastGoodValueOnFailure(t *testing.T) {
	var c ttlCache[int]
	if _, err := c.get(time.Hour, func() (int, error) { return 0, errors.New("first") }); err == nil {
		t.Fatal("with nothing cached the error must surface")
	}
	if v, err := c.get(0, func() (int, error) { return 7, nil }); err != nil || v != 7 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	if v, err := c.get(0, func() (int, error) { return 0, errors.New("later") }); err != nil || v != 7 {
		t.Fatalf("a later failure must keep the good value: v=%d err=%v", v, err)
	}
	calls := 0
	if v, _ := c.get(time.Hour, func() (int, error) { calls++; return 9, nil }); v != 7 || calls != 0 {
		t.Fatalf("within the TTL: v=%d calls=%d", v, calls)
	}
}

func TestParseCgroup(t *testing.T) {
	st := parseCgroup([]byte("low 0\nhigh 0\nmax 3\noom 3\noom_kill 2\nusage_usec 100\nnr_periods 200\nnr_throttled 50\nthrottled_usec 9\nbad\nnan x\n"))
	if st.OOMKills != 2 || st.ThrottledPct != 25 {
		t.Fatalf("%+v", st)
	}
	if parseCgroup(nil).ThrottledPct != 0 {
		t.Error("no periods must mean no throttling")
	}
}

func TestLoadCgroups_SkipsContainersThatFail(t *testing.T) {
	stubDocker(t, func(args ...string) ([]byte, error) {
		if args[1] == "bad" {
			return nil, errors.New("no such file")
		}
		return []byte("oom_kill 1\n"), nil
	})
	got := loadCgroups([]string{"good", "bad"})
	if got["good"].OOMKills != 1 || len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestUpdateResourceLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("runners:\n  base_path: "+dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = orig })
	put := func(body string) (int, error) {
		req := httptest.NewRequest(http.MethodPut, "/api/settings/resources", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		err := UpdateResourceLimits(echo.New().NewContext(req, rec))
		return rec.Code, err
	}
	for _, bad := range []string{`{"cpus":"","memory":"8g"}`, `{"cpus":"abc","memory":"8g"}`, `{"cpus":"2","memory":"lots"}`, `not json`} {
		if _, err := put(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if code, err := put(`{"cpus":"2","memory":"4g"}`); err != nil || code != http.StatusOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.Resources.CPUs != "2" || cfg.Runners.Resources.Memory != "4g" {
		t.Fatalf("%+v", cfg.Runners.Resources)
	}
	if cfg.AutoCleanAfter() != config.DefaultAutoCleanDays {
		t.Errorf("a request without the field must keep the value: %d", cfg.AutoCleanAfter())
	}
	if code, err := put(`{"cpus":"2","memory":"4g","auto_clean_days":0}`); err != nil || code != http.StatusOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if cfg, err = config.Load(path); err != nil || cfg.AutoCleanAfter() != 0 {
		t.Errorf("days=%d err=%v", cfg.AutoCleanAfter(), err)
	}
	if _, err := put(`{"cpus":"2","memory":"4g","auto_clean_days":-1}`); err == nil {
		t.Error("negative days accepted")
	}
}

func TestUpdateResourceLimits_ReportsContainersItCouldNotUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "runners:\n  base_path: " + dir + "\n  container_mode: true\n  volume_host_path: " + dir + "\n  job_docker_backend: none\n  items:\n    - {name: a, target_type: repo, target: o/r}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = orig })
	t.Setenv("PATH", t.TempDir())
	req := httptest.NewRequest(http.MethodPut, "/api/settings/resources", strings.NewReader(`{"cpus":"2","memory":"4g"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := UpdateResourceLimits(echo.New().NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a: ") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
