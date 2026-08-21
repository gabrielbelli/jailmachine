package machine

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewShareCanonicalisesAndTags(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "code")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// A symlink is followed and the path is cleaned. On macOS t.TempDir()
	// sits under /var/folders, whose /var is itself a symlink into
	// /private: that spelling is kept (see keepsFirmlinkSpelling), so the
	// answer is the path as written, not its /private form.
	s, err := NewShare(filepath.Join(dir, "link", ".", ""), false)
	if err != nil {
		t.Fatal(err)
	}
	if s.HostPath != sub {
		t.Errorf("HostPath = %q, want %q", s.HostPath, sub)
	}
	if s.GuestPath != s.HostPath {
		t.Errorf("identity-path rule broken: %q != %q", s.GuestPath, s.HostPath)
	}
	if s.Mode() != "rw" {
		t.Errorf("Mode = %q", s.Mode())
	}
	if !strings.HasPrefix(s.Tag, "jm-code-") || len(s.Tag) > MaxTagLen {
		t.Errorf("unexpected tag %q", s.Tag)
	}
}

func TestShareTagFitsAndIsStable(t *testing.T) {
	long := "/Users/belli/" + strings.Repeat("verylongdirectoryname", 4)
	for _, p := range []string{"/Users/belli", "/private/tmp", "/Volumes", long, "/a/b c/d.e"} {
		tag := ShareTag(p)
		if len(tag) > MaxTagLen {
			t.Errorf("tag for %q is %d bytes: %q", p, len(tag), tag)
		}
		if tag != ShareTag(p) {
			t.Errorf("tag for %q is not stable", p)
		}
		for _, r := range tag {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-~", r) {
				t.Errorf("tag %q for %q contains %q", tag, p, r)
			}
		}
	}
}

func TestShareTagSameBasenameDiffers(t *testing.T) {
	if ShareTag("/one/src") == ShareTag("/two/src") {
		t.Fatal("same basename in different directories must not share a tag")
	}
}

func TestCanonicalHostPathRejects(t *testing.T) {
	for _, p := range []string{"", "   ", "/", "/a\tb", "/a\nb"} {
		if _, err := CanonicalHostPath(p); !errors.Is(err, ErrInvalidShare) {
			t.Errorf("CanonicalHostPath(%q) = %v, want ErrInvalidShare", p, err)
		}
	}
}

func TestNormaliseSharesOrdersDedupesAndNests(t *testing.T) {
	in := []Share{
		{HostPath: "/Users/belli/code"},
		{HostPath: "/Volumes"},
		{HostPath: "/Users/belli"},
		{HostPath: "/Users/belli/"},                       // duplicate after cleaning
		{HostPath: "/Users/belli/secret", ReadOnly: true}, // different mode: kept
	}
	out, err := NormaliseShares(in)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range out {
		got = append(got, s.HostPath+":"+s.Mode())
	}
	want := []string{"/Users/belli:rw", "/Users/belli/secret:ro", "/Volumes:rw"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNormaliseSharesUniqueTags(t *testing.T) {
	out, err := NormaliseShares([]Share{{HostPath: "/one/src"}, {HostPath: "/two/src"}, {HostPath: "/three/src"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range out {
		if seen[s.Tag] {
			t.Fatalf("duplicate tag %q", s.Tag)
		}
		seen[s.Tag] = true
	}
}

// assignTags is the collision handler: distinct paths only clash on a hash
// collision, which no fixture can produce, so it is exercised directly.
func TestAssignTagsResolvesCollisions(t *testing.T) {
	in := []Share{
		{HostPath: "/a", Tag: "jm-x-deadbeef"},
		{HostPath: "/b", Tag: "jm-x-deadbeef"},
		{HostPath: "/c", Tag: "jm-x-deadbeef"},
		{HostPath: "/d", Tag: GuestConfTag},
		{HostPath: "/e", Tag: strings.Repeat("t", MaxTagLen)},
		{HostPath: "/f", Tag: strings.Repeat("t", MaxTagLen)},
	}
	out := assignTags(in)
	seen := map[string]bool{GuestConfTag: true}
	for _, s := range out {
		if s.Tag == "" || len(s.Tag) > MaxTagLen {
			t.Fatalf("tag %q for %s is %d bytes", s.Tag, s.HostPath, len(s.Tag))
		}
		if seen[s.Tag] {
			t.Fatalf("duplicate tag %q for %s", s.Tag, s.HostPath)
		}
		seen[s.Tag] = true
	}
}

func TestAddAndRemoveShare(t *testing.T) {
	list, err := NormaliseShares([]Share{{HostPath: "/Users/belli"}})
	if err != nil {
		t.Fatal(err)
	}
	list, err = AddShare(list, Share{HostPath: "/Volumes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("AddShare: %v", list)
	}
	// Re-adding the same path replaces it rather than duplicating.
	list, err = AddShare(list, Share{HostPath: "/Volumes", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || !list[1].ReadOnly {
		t.Fatalf("AddShare did not replace: %v", list)
	}
	list, found, err := RemoveShare(list, "/Volumes")
	if err != nil || !found || len(list) != 1 {
		t.Fatalf("RemoveShare: %v %v %v", list, found, err)
	}
	if _, found, _ = RemoveShare(list, "/Volumes"); found {
		t.Fatal("RemoveShare reported a share that is not there")
	}
}

func TestUsableSharesDropsVanishedPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, skipped := UsableShares([]Share{
		{HostPath: dir},
		{HostPath: filepath.Join(dir, "gone")},
		{HostPath: file},
	})
	if len(ok) != 1 || ok[0].HostPath != dir {
		t.Fatalf("ok = %v", ok)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v", skipped)
	}
	if !strings.Contains(skipped[0].Reason, "gone") || !strings.Contains(skipped[1].Reason, "not a directory") {
		t.Fatalf("reasons = %q, %q", skipped[0].Reason, skipped[1].Reason)
	}
}

func TestSharesTab(t *testing.T) {
	list, err := NormaliseShares([]Share{{HostPath: "/Volumes"}, {HostPath: "/private/tmp", ReadOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	tab := SharesTab(list)
	lines := strings.Split(strings.TrimSuffix(tab, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "#") {
		t.Fatalf("tab = %q", tab)
	}
	// Ordering is byte-wise on the guest path, so "/Volumes" precedes
	// "/private/tmp"; what matters is that a parent always precedes what
	// is nested inside it.
	if lines[1] != list[0].Tag+"\t/Volumes\trw" {
		t.Errorf("line 1 = %q", lines[1])
	}
	if lines[2] != list[1].Tag+"\t/private/tmp\tro" {
		t.Errorf("line 2 = %q", lines[2])
	}
}

// The macOS firmlinks (/etc, /tmp, /var -> /private/...) give every path
// under them two names. Under /var the system's own tools hand out the short
// one ($TMPDIR, "mktemp -d"), so a share must be exported under that name or
// "-v $(mktemp -d):/app" mounts an empty guest directory. /tmp and /etc stay
// resolved: a share at the guest's own /tmp or /etc would shadow it.
func TestShortFirmlinkPath(t *testing.T) {
	cases := map[string]string{
		"/private/var/folders":           "/var/folders",
		"/private/var/folders/5l/abc/T":  "/var/folders/5l/abc/T",
		"/private/tmp":                   "/private/tmp",
		"/private/tmp/build":             "/private/tmp/build",
		"/private/etc/conf":              "/private/etc/conf",
		"/private/var":                   "/private/var",
		"/Users/belli/code":              "/Users/belli/code",
		"/Volumes/disk/private/var/data": "/Volumes/disk/private/var/data",
	}
	for in, want := range cases {
		if got := shortFirmlinkPath(in); got != want {
			t.Errorf("shortFirmlinkPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalHostPathResolvesTheFirmlinkRoots(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the firmlink roots are a macOS layout")
	}
	// /tmp must not be shared at its own path: it would shadow the guest's
	// own /tmp. It canonicalises to /private/tmp, which is what jm shares.
	got, err := CanonicalHostPath("/tmp")
	if err != nil || got != "/private/tmp" {
		t.Errorf("CanonicalHostPath(/tmp) = %q, %v, want /private/tmp", got, err)
	}
	// A per-user temporary directory keeps the spelling the system hands
	// out, so a volume argument built from $TMPDIR finds it in the guest.
	tmp := filepath.Clean(os.TempDir())
	if !strings.HasPrefix(tmp, "/var/folders/") {
		t.Skipf("$TMPDIR is %q, not a per-user temporary directory", tmp)
	}
	if got, err := CanonicalHostPath(tmp); err != nil || got != tmp {
		t.Errorf("CanonicalHostPath(%q) = %q, %v", tmp, got, err)
	}
}
