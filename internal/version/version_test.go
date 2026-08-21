package version

import (
	"runtime"
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
