package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// run executes the root command with args against an isolated state root
// and returns stdout and the error.
func run(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	oldOut := stdout
	stdout = &buf
	defer func() { stdout = oldOut }()
	cmd := NewRootCmd()
	cmd.SetArgs(append([]string{"--state-root", root}, args...))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	return buf.String(), err
}

func seedRecord(t *testing.T, root, name string) *machine.Machine {
	t.Helper()
	m := machine.Defaults()
	m.Name = name
	m.Backend = backend.DefaultForHost()
	m.Created = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if err := machine.NewStore(root).Save(&m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestListEmpty(t *testing.T) {
	out, err := run(t, t.TempDir(), "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "NAME") || strings.Count(out, "\n") != 1 {
		t.Errorf("unexpected output:\n%s", out)
	}
	out, err = run(t, t.TempDir(), "--json", "list")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json list = %q, %v", out, err)
	}
}

func TestListAndInspectStopped(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "alpha")
	seedRecord(t, root, "beta")

	out, err := run(t, root, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "alpha") || !strings.Contains(lines[1], "stopped") || !strings.Contains(lines[1], "127.0.0.1:2222") {
		t.Errorf("unexpected list:\n%s", out)
	}

	out, err = run(t, root, "--json", "inspect", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var i struct {
		Name    string `json:"name"`
		State   string `json:"state"`
		SSH     string `json:"ssh"`
		Console string `json:"console"`
		Dir     string `json:"dir"`
	}
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	wantDir := filepath.Join(root, "machines", "alpha")
	if i.Name != "alpha" || i.State != "stopped" || i.SSH != "127.0.0.1:2222" || i.Dir != wantDir || i.Console != filepath.Join(wantDir, "console.log") {
		t.Errorf("unexpected inspect: %+v", i)
	}

	out, err = run(t, root, "inspect", "alpha")
	if err != nil || !strings.Contains(out, "State:") || !strings.Contains(out, "stopped") {
		t.Errorf("text inspect = %q, %v", out, err)
	}
}

func TestInspectMissing(t *testing.T) {
	_, err := run(t, t.TempDir(), "inspect")
	if err == nil || !strings.Contains(err.Error(), "jm init") {
		t.Errorf("err = %v", err)
	}
	_, err = run(t, t.TempDir(), "inspect", "Bad_Name")
	if err == nil || !strings.Contains(err.Error(), "invalid machine name") {
		t.Errorf("err = %v", err)
	}
}

func TestStartAndStopMissing(t *testing.T) {
	for _, c := range []string{"start", "stop", "ssh"} {
		if _, err := run(t, t.TempDir(), c); err == nil {
			t.Errorf("%s on a missing machine should fail", c)
		}
	}
}

func TestStopStoppedIsNoop(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "jailmachine")
	out, err := run(t, root, "stop")
	if err != nil || !strings.Contains(out, "not running") {
		t.Errorf("stop = %q, %v", out, err)
	}
}

func TestRmConverges(t *testing.T) {
	root := t.TempDir()
	out, err := run(t, root, "rm", "ghost")
	if err != nil || !strings.Contains(out, "nothing to remove") {
		t.Errorf("rm ghost = %q, %v", out, err)
	}
	// Half-initialised directory without a record is removed too.
	dir := filepath.Join(root, "machines", "half")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "rm", "half"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists", dir)
	}
}

func TestInitRejectsBadFlags(t *testing.T) {
	root := t.TempDir()
	cases := [][]string{
		{"init", "--cpus", "0"},
		{"init", "--memory", "64"},
		{"init", "--disk", "0"},
		{"init", "--ssh-port", "70000"},
		{"init", "--image", "nope:1"},
		{"init", "--image", ":1"},
		{"init", "Bad"},
	}
	for _, args := range cases {
		if _, err := run(t, root, args...); err == nil {
			t.Errorf("%v should fail", args)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "machines")); len(entries) != 0 {
		t.Errorf("validation failures must not create state: %v", entries)
	}
}

func TestInitRefusesExisting(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "jailmachine")
	_, err := run(t, root, "init")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v", err)
	}
}

func TestSplitSSHArgs(t *testing.T) {
	exists := func(n string) bool { return n == "dev" }
	name, rest := splitSSHArgs(nil, exists)
	if name != machine.DefaultName || len(rest) != 0 {
		t.Errorf("nil: %q %v", name, rest)
	}
	name, rest = splitSSHArgs([]string{"dev", "ls", "-la"}, exists)
	if name != "dev" || strings.Join(rest, " ") != "ls -la" {
		t.Errorf("dev: %q %v", name, rest)
	}
	name, rest = splitSSHArgs([]string{"uname", "-a"}, exists)
	if name != machine.DefaultName || strings.Join(rest, " ") != "uname -a" {
		t.Errorf("cmd: %q %v", name, rest)
	}
}
