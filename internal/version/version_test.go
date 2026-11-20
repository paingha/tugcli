package version

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// setLinkerVars sets the -ldflags variables for one test.
func setLinkerVars(t *testing.T, v, c, d string) {
	t.Helper()
	oldV, oldC, oldD := version, commit, date
	version, commit, date = v, c, d
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })
}

func TestGetUsesLinkerValues(t *testing.T) {
	setLinkerVars(t, "v1.2.3", "abc123", "2026-11-20T12:00:00Z")

	want := Info{
		Version:   "v1.2.3",
		Commit:    "abc123",
		Date:      "2026-11-20T12:00:00Z",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if got := Get(); got != want {
		t.Fatalf("Get() = %+v, want %+v", got, want)
	}
	if got := String(); got != "v1.2.3" {
		t.Fatalf("String() = %q, want %q", got, "v1.2.3")
	}
}

func TestGetFallsBack(t *testing.T) {
	setLinkerVars(t, "", "", "")

	// Without linker values, Get uses what the toolchain embedded, and "dev"
	// when there is no module version (as in a test binary).
	want := Info{
		Version:   "dev",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			want.Version = v
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				want.Commit = s.Value
			case "vcs.time":
				want.Date = s.Value
			}
		}
	}
	if got := Get(); got != want {
		t.Fatalf("Get() = %+v, want %+v", got, want)
	}
	if got := String(); got != want.Version {
		t.Fatalf("String() = %q, want %q", got, want.Version)
	}
}

func TestGetLinkerValuesWinOverBuildInfo(t *testing.T) {
	// A commit or date set at link time is never replaced by VCS settings.
	setLinkerVars(t, "", "linked-commit", "linked-date")
	got := Get()
	if got.Commit != "linked-commit" || got.Date != "linked-date" {
		t.Fatalf("Get() = %+v, want Commit %q and Date %q", got, "linked-commit", "linked-date")
	}
	if got.Version == "" {
		t.Fatal("Get().Version is empty, want a fallback")
	}
}

func TestIsRelease(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.4.0": true, "v1.12.3": true,
		"dev": false, "0.4.0": false, "v0.4.0-rc.1": false,
		"v0.0.0-20261120195036-d92e7b0a68cf+dirty": false,
	} {
		if got := IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", v, got, want)
		}
	}
}
