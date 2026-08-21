package version

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	if Version != "dev" || Commit != "none" || Date != "unknown" {
		t.Fatalf("unexpected defaults: %s", Full())
	}
	if got := Full(); got != "jm dev (none, unknown)" {
		t.Fatalf("Full = %q", got)
	}
}

func TestStringMentionsEverything(t *testing.T) {
	old := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = old })
	s := String()
	for _, want := range []string{"jm version 1.2.3", "commit:", "built:", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(s, want) {
			t.Errorf("String() lacks %q:\n%s", want, s)
		}
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("String() should end with a newline")
	}
	if info := Current(); info.Version != "1.2.3" || info.GoVersion != runtime.Version() {
		t.Errorf("Current = %+v", info)
	}
}

func TestFromBuildInfo(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldV, oldC, oldD })
	Version, Commit, Date = "dev", "none", "unknown"
	fromBuildInfo(func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v0.1.1"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abcdef1234567890"},
				{Key: "vcs.time", Value: "2026-08-21T10:00:00Z"},
			},
		}, true
	})
	if Version != "0.1.1" || Commit != "abcdef1" || Date != "2026-08-21T10:00:00Z" {
		t.Fatalf("got %s %s %s", Version, Commit, Date)
	}
	// The linker must win over build info.
	Version, Commit, Date = "9.9.9", "deadbee", "2020-01-01T00:00:00Z"
	fromBuildInfo(func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v0.0.1"}}, true
	})
	if Version != "9.9.9" {
		t.Fatalf("linker value overwritten: %s", Version)
	}
}
