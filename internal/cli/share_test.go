package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func TestParseMount(t *testing.T) {
	dir := t.TempDir()
	real, err := machine.CanonicalHostPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		path string
		ro   bool
	}{
		{dir, real, false},
		{dir + ":ro", real, true},
		{dir + ":rw", real, false},
		{dir + "/.", real, false},
	}
	for _, c := range cases {
		s, err := parseMount(c.in)
		if err != nil {
			t.Fatalf("parseMount(%q): %v", c.in, err)
		}
		if s.HostPath != c.path || s.GuestPath != c.path || s.ReadOnly != c.ro {
			t.Errorf("parseMount(%q) = %+v, want %s ro=%v", c.in, s, c.path, c.ro)
		}
	}
}

func TestParseMountRejectsHostGuestPairs(t *testing.T) {
	if _, err := parseMount("/work:/app"); err == nil || !strings.Contains(err.Error(), "host path alone") {
		t.Fatalf("parseMount(/work:/app) = %v", err)
	}
	if _, err := parseMount(""); err == nil {
		t.Fatal("empty mount accepted")
	}
	if _, err := parseMount("/"); err == nil {
		t.Fatal("sharing / accepted")
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for in, want := range map[string]string{
		"~":        home,
		"~/code":   filepath.Join(home, "code"),
		"/x/~/y":   "/x/~/y",
		"~user/x":  "~user/x",
		"relative": "relative",
	} {
		got, err := expandTilde(in)
		if err != nil || got != want {
			t.Errorf("expandTilde(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestInitOptsShares(t *testing.T) {
	dir := t.TempDir()
	real, err := machine.CanonicalHostPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	got, err := initOpts{noMounts: true}.shares()
	if err != nil || got != nil {
		t.Fatalf("--no-mounts: %v %v", got, err)
	}
	if _, err := (initOpts{noMounts: true, mounts: []string{dir}}).shares(); err == nil {
		t.Fatal("--no-mounts with --mount accepted")
	}
	// --mount adds to the defaults, as "jm set --mount" adds to a machine's
	// existing set; it does not replace them.
	got, err = initOpts{mounts: []string{dir + ":ro"}}.shares()
	if err != nil {
		t.Fatal(err)
	}
	if !hasShare(got, real, true) {
		t.Fatalf("--mount %s:ro missing from %+v", real, got)
	}
	base, err := defaultShares()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range base {
		if !hasShare(got, s.HostPath, s.ReadOnly) {
			t.Fatalf("--mount dropped the default share %s from %+v", s.HostPath, got)
		}
	}
	// The default set is whatever of the default roots this host has, and
	// it always includes the home directory.
	got, err = initOpts{}.shares()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	var found bool
	for _, s := range got {
		if s.HostPath == home || strings.HasPrefix(home, s.HostPath+"/") {
			found = true
		}
		if s.GuestPath != s.HostPath {
			t.Errorf("identity-path rule broken: %+v", s)
		}
	}
	if !found {
		t.Errorf("default shares %v do not cover the home directory %s", got, home)
	}
	// ... and the per-user temporary directory, where "mktemp -d" and
	// os.MkdirTemp put things: without it "-v $(mktemp -d):/app" would
	// mount an empty guest directory (ADR 0007).
	//
	// This half is macOS-shaped, like the feature: userTempRoot only
	// recognises the /var/folders/<hash> layout, so on a Linux CI runner
	// $TMPDIR is /tmp, no default root covers it, and there is nothing to
	// assert. jm has no Linux backend, so that is correct rather than a
	// gap -- but the assertion has to say so, or "go test ./..." on
	// ubuntu-latest fails on a platform the feature does not target.
	if userTempRoot() == "" {
		t.Skipf("no per-user temp root on %s ($TMPDIR=%s); the default-share set is macOS-shaped", runtime.GOOS, os.TempDir())
	}
	tmp, err := machine.CanonicalHostPath(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, s := range got {
		if s.HostPath == tmp || strings.HasPrefix(tmp, s.HostPath+"/") {
			found = true
		}
	}
	if !found {
		t.Errorf("default shares %v do not cover $TMPDIR %s", got, tmp)
	}
}

func TestApplyMounts(t *testing.T) {
	base, err := machine.NormaliseShares([]machine.Share{{HostPath: "/Volumes"}, {HostPath: "/private/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyMounts(base, []string{"/work:ro"}, []string{"/Volumes"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, s := range got {
		paths = append(paths, s.HostPath+":"+s.Mode())
	}
	if strings.Join(paths, ",") != "/private/tmp:rw,/work:ro" {
		t.Fatalf("applyMounts = %v", paths)
	}
	if _, err := applyMounts(base, nil, []string{"/nowhere"}); err == nil {
		t.Fatal("unmounting a path that is not shared should fail")
	}
}

func TestApplyMountsRejectsTooMany(t *testing.T) {
	var add []string
	for i := 0; i <= machine.MaxShares; i++ {
		add = append(add, filepath.Join("/m", string(rune('a'+i))))
	}
	if _, err := applyMounts(nil, add, nil); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("err = %v", err)
	}
}

func TestSameShares(t *testing.T) {
	a, _ := machine.NormaliseShares([]machine.Share{{HostPath: "/Volumes"}})
	b, _ := machine.NormaliseShares([]machine.Share{{HostPath: "/Volumes"}})
	c, _ := machine.NormaliseShares([]machine.Share{{HostPath: "/Volumes", ReadOnly: true}})
	if !sameShares(a, b) {
		t.Error("identical sets differ")
	}
	if sameShares(a, c) {
		t.Error("read-only change not noticed")
	}
	if sameShares(a, nil) {
		t.Error("empty set matches a non-empty one")
	}
}

func TestSetValidateShares(t *testing.T) {
	m := machine.Defaults()
	m.Shares, _ = machine.NormaliseShares([]machine.Share{{HostPath: "/Volumes"}})
	c, err := setOpts{mount: []string{"/work"}}.validate(&m)
	if err != nil {
		t.Fatal(err)
	}
	if !c.sharesSet || !c.needsStopped() || len(c.shares) != 2 {
		t.Fatalf("changes = %+v", c)
	}
	if _, err := (setOpts{unmount: []string{"/work"}}).validate(&m); err == nil {
		t.Fatal("unmounting an unshared path accepted")
	}
}

// hasShare reports whether a share set contains a path with a given mode.
func hasShare(list []machine.Share, hostPath string, readOnly bool) bool {
	for _, s := range list {
		if s.HostPath == hostPath && s.ReadOnly == readOnly {
			return true
		}
	}
	return false
}
