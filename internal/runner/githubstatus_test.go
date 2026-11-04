package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func ptr(b bool) *bool { return &b }

// The three states must round-trip unchanged: 'could not determine' must differ from 'looked up and confirmed not registered'
func TestGitHubStatus_RoundTripsThreeStates(t *testing.T) {
	cases := []struct {
		name      string
		write     *bool
		writeBusy *bool
		writeErr  string
	}{
		{name: "已登记且空闲", write: ptr(true), writeBusy: ptr(false)},
		{name: "已登记且忙碌", write: ptr(true), writeBusy: ptr(true)},
		{name: "确实没登记", write: ptr(false), writeBusy: nil},
		{name: "查不出来", write: nil, writeBusy: nil, writeErr: "GitHub 返回 401：令牌无效或已过期"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const at0 = "2026-09-20T09:00:00Z"
			if err := WriteGitHubStatus(dir, GitHubStatus{
				Registered: tc.write, Busy: tc.writeBusy, Error: tc.writeErr, LastCheck: at0,
			}); err != nil {
				t.Fatal(err)
			}
			st := ReadGitHubStatus(dir)
			got, gotBusy, at, gotErr := st.Registered, st.Busy, st.LastCheck, st.Error
			if at != at0 {
				t.Fatalf("check time should be stored back unchanged, wrote %q read back %q", at0, at)
			}
			switch {
			case tc.writeBusy == nil && gotBusy != nil:
				t.Fatalf("busy state write unknown, read back %v", *gotBusy)
			case tc.writeBusy != nil && gotBusy == nil:
				t.Fatalf("busy state wrote %v, read back unknown", *tc.writeBusy)
			case tc.writeBusy != nil && *gotBusy != *tc.writeBusy:
				t.Fatalf("busy state wrote %v, read back %v", *tc.writeBusy, *gotBusy)
			}
			switch {
			case tc.write == nil && got != nil:
				t.Fatalf("write unknown, read back %v", *got)
			case tc.write != nil && got == nil:
				t.Fatalf("wrote %v, read back unknown", *tc.write)
			case tc.write != nil && *got != *tc.write:
				t.Fatalf("wrote %v, read back %v", *tc.write, *got)
			}
			if gotErr != tc.writeErr {
				t.Fatalf("reason note wrote %q read back %q", tc.writeErr, gotErr)
			}
		})
	}
}

// In files written by the old version, registered was a plain bool with no error field; its meaning must not change
func TestGitHubStatus_ReadsLegacyFormat(t *testing.T) {
	for _, legacy := range []struct {
		body string
		want bool
	}{
		{`{"registered":true,"last_check":"2026-01-01T00:00:00Z"}`, true},
		{`{"registered":false,"last_check":"2026-01-01T00:00:00Z"}`, false},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, GitHubStatusFile), []byte(legacy.body), 0644); err != nil {
			t.Fatal(err)
		}
		st := ReadGitHubStatus(dir)
		got, gotBusy, at, gotErr := st.Registered, st.Busy, st.LastCheck, st.Error
		if got == nil || *got != legacy.want {
			t.Fatalf("legacy format %s should read %v, got %v", legacy.body, legacy.want, got)
		}
		// Old files have no busy field: that means 'unknown', and must not be treated as 'not busy'
		if gotBusy != nil {
			t.Fatalf("legacy format has no busy field, should read as unknown, got %v", *gotBusy)
		}
		if at != "2026-01-01T00:00:00Z" || gotErr != "" {
			t.Fatalf("legacy format read at=%q err=%q", at, gotErr)
		}
		// Old files also lack public / visibility_checked_at: that means 'visibility not checked yet',
		// reading it as false would mark every upgraded deployment as 'private'
		if st.Public != nil || st.VisibilityCheckedAt != "" {
			t.Fatalf("legacy format has no visibility field, should read as unchecked, got %v/%q", st.Public, st.VisibilityCheckedAt)
		}
	}
}

// Visibility is the third tri-state field; like the two above, 'unknown' must not be stored as 'no'
func TestGitHubStatus_RoundTripsVisibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		public *bool
	}{
		{"公开", ptr(true)},
		{"私有", ptr(false)},
		{"不知道", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const at = "2026-09-20T09:00:00Z"
			if err := WriteGitHubStatus(dir, GitHubStatus{Public: tc.public, VisibilityCheckedAt: at}); err != nil {
				t.Fatal(err)
			}
			st := ReadGitHubStatus(dir)
			switch {
			case tc.public == nil && st.Public != nil:
				t.Fatalf("write unknown, read back %v", *st.Public)
			case tc.public != nil && st.Public == nil:
				t.Fatalf("wrote %v, read back unknown", *tc.public)
			case tc.public != nil && *st.Public != *tc.public:
				t.Fatalf("wrote %v, read back %v", *tc.public, *st.Public)
			}
			if st.VisibilityCheckedAt != at {
				t.Fatalf("visibility check time wrote %q read back %q", at, st.VisibilityCheckedAt)
			}
		})
	}
}

// No file = never checked, which differs from 'checked but failed': the latter has a check time
func TestGitHubStatus_MissingFileIsNeverChecked(t *testing.T) {
	st := ReadGitHubStatus(t.TempDir())
	if st != (GitHubStatus{}) {
		t.Fatalf("with no state file everything should be empty, got %+v", st)
	}
}

// Template helper methods: {{if .RegisteredOnGitHub}} only checks whether a pointer is nil,
// a pointer to false is also truthy, so templates must use these three methods
func TestRunnerInfo_GitHubTriStateHelpers(t *testing.T) {
	cases := []struct {
		name             string
		v                *bool
		yes, no, unknown bool
	}{
		{"已登记", ptr(true), true, false, false},
		{"没登记", ptr(false), false, true, false},
		{"未知", nil, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := RunnerInfo{RegisteredOnGitHub: tc.v}
			if info.GitHubYes() != tc.yes || info.GitHubNo() != tc.no || info.GitHubUnknown() != tc.unknown {
				t.Fatalf("yes/no/unknown = %v/%v/%v, want %v/%v/%v",
					info.GitHubYes(), info.GitHubNo(), info.GitHubUnknown(), tc.yes, tc.no, tc.unknown)
			}
		})
	}
}

// GitHubPublic is also a *bool. {{if .GitHubPublic}} would make every private repo show
// the 'public repo' badge; this field exists to pick out public repos, so a false positive defeats its purpose.
func TestRunnerInfo_GitHubPublicHelper(t *testing.T) {
	cases := []struct {
		name string
		v    *bool
		want bool
	}{
		{"公开", ptr(true), true},
		{"私有", ptr(false), false},
		{"未知", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (RunnerInfo{GitHubPublic: tc.v}).GitHubPublicYes(); got != tc.want {
				t.Fatalf("GitHubPublicYes() = %v, expected %v", got, tc.want)
			}
		})
	}
}

// GitHubBusy is also *bool; writing {{if .GitHubBusy}} in the template would show "idle" as "busy"
func TestRunnerInfo_GitHubBusyHelper(t *testing.T) {
	cases := []struct {
		name string
		v    *bool
		want bool
	}{
		{"忙碌", ptr(true), true},
		{"空闲", ptr(false), false},
		{"未知", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (RunnerInfo{GitHubBusy: tc.v}).GitHubBusyYes(); got != tc.want {
				t.Fatalf("GitHubBusyYes() = %v, expected %v", got, tc.want)
			}
		})
	}
}
