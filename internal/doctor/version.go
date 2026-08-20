package doctor

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a dotted numeric version; missing components are zero.
type Version struct {
	Major, Minor, Patch int
}

// String renders the version as Major.Minor.Patch.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseVersion pulls the first dotted number out of a tool's --version
// output. It copes with the shapes jm meets: "QEMU emulator version
// 11.1.0", "podman version 6.1.0", "gvproxy version v0.8.9", and bare
// "5.5.2-dev" or "v1.2". The leading line is searched word by word so a
// copyright year later in the output is never mistaken for the version.
func ParseVersion(out string) (Version, bool) {
	for _, word := range strings.Fields(out) {
		word = strings.TrimPrefix(word, "v")
		if v, ok := parseDotted(word); ok {
			return v, true
		}
	}
	return Version{}, false
}

// parseDotted parses "X", "X.Y" or "X.Y.Z" with an optional suffix after
// "-", "+" or "~" (e.g. "6.1.0-dev", "8.2.2+ds"). A bare integer is only
// accepted when it is small enough not to be a year.
func parseDotted(word string) (Version, bool) {
	if i := strings.IndexAny(word, "-+~"); i >= 0 {
		word = word[:i]
	}
	parts := strings.Split(word, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return Version{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return Version{}, false
		}
		nums[i] = n
	}
	if len(parts) == 1 && nums[0] >= 1000 {
		return Version{}, false
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, true
}
