package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"QEMU emulator version 11.1.0\nCopyright (c) 2003-2025 Fabrice Bellard", Version{11, 1, 0}, true},
		{"QEMU emulator version 8.2.2 (Debian 1:8.2.2+ds-0ubuntu1)", Version{8, 2, 2}, true},
		{"podman version 6.1.0", Version{6, 1, 0}, true},
		{"podman version 5.5.2-dev", Version{5, 5, 2}, true},
		{"gvproxy version v0.8.9", Version{0, 8, 9}, true},
		{"v1.2", Version{1, 2, 0}, true},
		{"7", Version{7, 0, 0}, true},
		{"Copyright 2025 someone", Version{}, false},
		{"", Version{}, false},
		{"no numbers here", Version{}, false},
		{"1.2.3.4 too many", Version{}, false},
	}
	for _, c := range cases {
		got, ok := ParseVersion(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if (Version{11, 1, 0}).String() != "11.1.0" {
		t.Error("String()")
	}
}

func TestCheckVersionThresholds(t *testing.T) {
	ctx := context.Background()
	run := func(out string, err error) Runner {
		return func(context.Context, string, ...string) (string, error) { return out, err }
	}
	if r := checkVersion(ctx, run("podman version 6.1.0", nil), "podman", "/x/podman", nil, 5, "up"); r.Status != OK || !strings.Contains(r.Detail, "6.1.0") {
		t.Errorf("new enough: %+v", r)
	}
	if r := checkVersion(ctx, run("podman version 4.9.0", nil), "podman", "/x/podman", nil, 5, "brew upgrade podman"); r.Status != Fail || !strings.Contains(r.Fix, "brew upgrade podman") {
		t.Errorf("too old: %+v", r)
	}
	if r := checkVersion(ctx, run("garbage", nil), "podman", "/x/podman", nil, 5, "up"); r.Status != Warn {
		t.Errorf("unparseable: %+v", r)
	}
	if r := checkVersion(ctx, run("", errors.New("exec: boom")), "podman", "/x/podman", nil, 5, "up"); r.Status != Fail || !strings.Contains(r.Detail, "boom") {
		t.Errorf("exec failure: %+v", r)
	}
}

func TestHasAccel(t *testing.T) {
	out := "Accelerators supported in QEMU binary:\nhvf\ntcg"
	if !hasAccel(out, "hvf") || hasAccel(out, "kvm") {
		t.Error("hasAccel")
	}
}

func fakeReport() Report {
	return Report{OS: "darwin", Arch: "arm64", StateRoot: "/tmp/jm", Results: []Result{
		{Name: "host", Status: OK, Detail: "darwin/arm64"},
		{Name: "podman version", Status: Fail, Detail: "4.9.0 at /opt/homebrew/bin/podman", Fix: "brew upgrade podman (need 5.x or newer)"},
		{Name: "xz", Status: Warn, Detail: "not found on PATH", Fix: "brew install xz"},
		{Name: "machine jailmachine", Status: OK, Detail: "stopped"},
	}}
}

func TestReportCounts(t *testing.T) {
	r := fakeReport()
	ok, warn, fail := r.Counts()
	if ok != 2 || warn != 1 || fail != 1 || !r.Failed() {
		t.Errorf("counts = %d %d %d failed=%v", ok, warn, fail, r.Failed())
	}
	if (Report{Results: []Result{{Status: Warn}}}).Failed() {
		t.Error("warn alone must not fail")
	}
}

func TestWriteTable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, fakeReport()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "STATUS") {
		t.Errorf("header: %q", lines[0])
	}
	for _, want := range []string{
		"[ ok ]  host",
		"[FAIL]  podman version",
		"fix: brew upgrade podman (need 5.x or newer)",
		"[warn]  xz",
		"fix: brew install xz",
		"2 ok, 1 warning(s), 1 failure(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	// OK rows carry no fix line.
	if strings.Count(out, "fix:") != 2 {
		t.Errorf("expected exactly two fix lines:\n%s", out)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, fakeReport()); err != nil {
		t.Fatal(err)
	}
	var got struct {
		OS     string `json:"os"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Fix    string `json:"fix"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, buf.String())
	}
	if got.OS != "darwin" || len(got.Checks) != 4 || got.Checks[1].Status != "fail" || got.Checks[1].Fix == "" || got.Checks[0].Fix != "" {
		t.Errorf("unexpected json: %s", buf.String())
	}
}

func TestCheckStateRoot(t *testing.T) {
	root := t.TempDir()
	if r := checkStateRoot(root); r.Status != OK {
		t.Errorf("existing temp dir: %+v", r)
	}
	missing := filepath.Join(root, "a", "b", "c")
	if r := checkStateRoot(missing); r.Status != OK || !strings.Contains(r.Detail, "will be created") {
		t.Errorf("missing under writable parent: %+v", r)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkStateRoot(file); r.Status != Fail {
		t.Errorf("regular file: %+v", r)
	}
}

func TestCheckSocketPaths(t *testing.T) {
	if r := checkSocketPaths("/tmp/jm"); r.Status != OK {
		t.Errorf("short root: %+v", r)
	}
	deep := "/" + strings.Repeat("x", 120)
	if r := checkSocketPaths(deep); r.Status != Warn || !strings.Contains(r.Fix, "--state-root") {
		t.Errorf("deep root: %+v", r)
	}
}

// Run with a fake runner and no machines must not panic and must include
// the host and state-root checks regardless of what is installed.
func TestRunSmoke(t *testing.T) {
	run := func(context.Context, string, ...string) (string, error) { return "x version 99.0.0\nhvf", nil }
	r := Run(context.Background(), Options{StateRoot: t.TempDir(), Run: run})
	names := map[string]bool{}
	for _, c := range r.Results {
		names[c.Name] = true
	}
	for _, want := range []string{"host", "backend qemu", "gvproxy", "podman", "ssh", "ssh-keygen", "xz", "state root", "socket paths"} {
		if !names[want] && !names[want+" version"] {
			t.Errorf("missing check %q in %v", want, names)
		}
	}
}
