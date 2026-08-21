// Package version holds the build identity stamped into jm by the linker
// (-ldflags "-X github.com/gabrielbelli/jailmachine/internal/version.Version=...").
// Goreleaser sets all three; a plain "go build" leaves the defaults, which
// is how "jm version" tells a release from a development build.
package version

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by the linker. Keep them as plain string variables: -X only works on
// uninitialised or string-literal package-level vars.
var (
	// Version is the release tag without a leading "v", or "dev".
	Version = "dev"
	// Commit is the short git hash the binary was built from, or "none".
	Commit = "none"
	// Date is the build time in RFC 3339, or "unknown".
	Date = "unknown"
)

// fromBuildInfo fills the gaps left by an unstamped build. "go install
// module@vX.Y.Z" has no linker flags, but the module version is recorded in
// the binary; a "go build" inside a git checkout records the VCS revision
// and time instead. Called once at start-up so the linker still wins.
func init() { fromBuildInfo(debug.ReadBuildInfo) }

func fromBuildInfo(read func() (*debug.BuildInfo, bool)) {
	bi, ok := read()
	if !ok {
		return
	}
	if Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "none" && len(s.Value) >= 7 {
				Commit = s.Value[:7]
			}
		case "vcs.time":
			if Date == "unknown" && s.Value != "" {
				Date = s.Value
			}
		}
	}
}

// Short returns the version on its own, for "jm --version".
func Short() string { return Version }

// Full returns a one-line identity: "jm 0.1.0 (abc1234, 2026-08-20T10:00:00Z)".
func Full() string {
	return fmt.Sprintf("jm %s (%s, %s)", Version, Commit, Date)
}

// Platform is the host Go runtime's os/arch, e.g. "darwin/arm64".
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Write prints the multi-line report used by "jm version".
func Write(w io.Writer) error {
	_, err := fmt.Fprint(w, String())
	return err
}

// String is the multi-line report used by "jm version".
func String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "jm version %s\n", Version)
	fmt.Fprintf(&b, "  commit:     %s\n", Commit)
	fmt.Fprintf(&b, "  built:      %s\n", Date)
	fmt.Fprintf(&b, "  go version: %s\n", runtime.Version())
	fmt.Fprintf(&b, "  os/arch:    %s\n", Platform())
	return b.String()
}

// Info is the machine-readable form for "jm version --json".
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Current returns the Info for this binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}
