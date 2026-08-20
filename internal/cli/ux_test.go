package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func TestResolveDefault(t *testing.T) {
	cases := []struct {
		existing []string
		name     string
		note     bool
		err      bool
		usage    bool
	}{
		{nil, "", false, true, false},
		{[]string{"jailmachine"}, "jailmachine", false, false, false},
		{[]string{"dev", "jailmachine"}, "jailmachine", false, false, false},
		{[]string{"dev"}, "dev", true, false, false},
		{[]string{"dev", "ci"}, "", false, true, true},
	}
	for _, c := range cases {
		name, note, err := resolveDefault(c.existing)
		if name != c.name || (note != "") != c.note || (err != nil) != c.err || (err != nil && isUsage(err) != c.usage) {
			t.Errorf("resolveDefault(%v) = %q, %q, %v", c.existing, name, note, err)
		}
		if c.usage && (!strings.Contains(err.Error(), "ci, dev") || exitCode(err) != ExitUsage) {
			t.Errorf("ambiguous error should list candidates and exit 2: %v (%d)", err, exitCode(err))
		}
	}
}

// With no "jailmachine" and exactly one machine, commands use it and say so;
// with several, they refuse with exit 2 and list them.
func TestDefaultMachineFallback(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "dev")
	out, err := run(t, root, "inspect")
	if err != nil || !strings.Contains(out, "==> using dev (the only machine)") || !strings.Contains(out, "Name:") {
		t.Fatalf("inspect = %q, %v", out, err)
	}
	out, err = run(t, root, "--json", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	var i struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &i); err != nil || i.Name != "dev" {
		t.Errorf("json inspect = %q, %v", out, err)
	}
	seedRecord(t, root, "ci")
	_, err = run(t, root, "stop")
	if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "ci, dev") {
		t.Errorf("ambiguous stop = %v (exit %d)", err, exitCode(err))
	}
	// An explicit name is never second-guessed.
	if _, err := run(t, root, "stop", "ci"); err != nil {
		t.Errorf("stop ci: %v", err)
	}
	// init always means the default name, even when another machine exists.
	_, err = run(t, root, "init", "--cpus", "0")
	if err == nil || !strings.Contains(err.Error(), "--cpus") {
		t.Errorf("init should validate flags for the default name: %v", err)
	}
}

func TestFormatError(t *testing.T) {
	cases := []struct {
		command, name string
		err           error
		want          string
	}{
		{"start", "jailmachine",
			machine.NewStageError(machine.StageBackend, "see /x/qemu.log and /x/console.log", errors.New("qemu exited")),
			"jm: start jailmachine: backend: qemu exited\n  hint: see /x/qemu.log and /x/console.log"},
		{"stop", "dev", errors.New("boom"), "jm: stop dev: boom"},
		{"list", "", errors.New("boom"), "jm: list: boom"},
		{"", "", errors.New("boom"), "jm: boom"},
		{"ssh", "dev", withHint(errors.New("dev is not running"), "run 'jm start dev'"),
			"jm: ssh dev: dev is not running\n  hint: run 'jm start dev'"},
		{"init", "", usagef("--cpus must be at least 1"),
			"jm: init: --cpus must be at least 1\n  hint: run 'jm init --help'"},
		// A stage error wrapped further up still reports stage and hint.
		{"start", "dev", withHint(machine.NewStageError(machine.StageNetwork, "see gvproxy.log", errors.New("died")), "outer"),
			"jm: start dev: network: died\n  hint: see gvproxy.log"},
	}
	for _, c := range cases {
		if got := formatError(c.command, c.name, c.err); got != c.want {
			t.Errorf("formatError(%q, %q, %v):\n got %q\nwant %q", c.command, c.name, c.err, got, c.want)
		}
	}
	if formatError("x", "y", nil) != "" {
		t.Error("nil error should format to empty")
	}
}

func TestExitCodes(t *testing.T) {
	if exitCode(nil) != ExitOK || exitCode(errors.New("x")) != ExitFailure || exitCode(usagef("x")) != ExitUsage {
		t.Error("basic exit codes wrong")
	}
	root := t.TempDir()
	seedRecord(t, root, "jailmachine")
	usageCases := [][]string{
		{"bogus"},
		{"list", "extra"},
		{"inspect", "a", "b"},
		{"inspect", "Bad_Name"},
		{"--no-such-flag", "list"},
		{"init", "--memory", "1"},
		{"init", "--image", ":x"},
		{"env", "--shell", "csh"},
	}
	for _, args := range usageCases {
		_, err := run(t, root, args...)
		if err == nil || exitCode(err) != ExitUsage {
			t.Errorf("%v: err = %v, exit %d, want 2", args, err, exitCode(err))
		}
	}
	_, err := run(t, t.TempDir(), "inspect", "ghost")
	if err == nil || exitCode(err) != ExitFailure {
		t.Errorf("missing machine should be a failure (1), got %v (%d)", err, exitCode(err))
	}
}

func TestQuietSuppressesStageLines(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "dev")
	out, err := run(t, root, "-q", "stop")
	if err != nil || strings.Contains(out, "==>") {
		t.Errorf("quiet stop = %q, %v", out, err)
	}
	// Data output is not silenced.
	out, err = run(t, root, "-q", "list")
	if err != nil || !strings.Contains(out, "dev") {
		t.Errorf("quiet list = %q, %v", out, err)
	}
	if !quiet {
		t.Error("quiet flag not recorded")
	}
	if showProgress() {
		t.Error("progress must be off under --quiet")
	}
	run(t, root, "list")
	if quiet {
		t.Error("quiet flag leaked into the next run")
	}
}

func TestListColumns(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "dev")
	out, err := run(t, root, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if strings.Join(strings.Fields(lines[0]), " ") != "NAME STATE CPUS MEMORY DISK SSH PORTS" {
		t.Errorf("header = %q", lines[0])
	}
	if f := strings.Fields(lines[1]); len(f) != 9 || f[0] != "dev" || f[1] != "stopped" || f[2] != "4" || f[3] != "4096" || f[5] != "64" || f[8] != "0" {
		t.Errorf("row = %q", lines[1])
	}
	out, err = run(t, root, "--json", "list")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("json list = %q, %v", out, err)
	}
	for _, k := range []string{"name", "state", "cpus", "memory_mib", "disk_gib", "ssh", "ports"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("json list lacks %q", k)
		}
	}
}

func TestHelpHasQuickstartAndExamples(t *testing.T) {
	out, err := run(t, t.TempDir(), "--help")
	if err != nil || !strings.Contains(out, "Quickstart") || !strings.Contains(out, "--os=linux") || !strings.Contains(out, "Examples:") {
		t.Errorf("root help = %q, %v", out, err)
	}
	for _, c := range []string{"init", "start", "stop", "ssh", "inspect", "rm", "list", "env", "ports"} {
		out, err := run(t, t.TempDir(), c, "--help")
		if err != nil || !strings.Contains(out, "Examples:") {
			t.Errorf("%s --help lacks Examples: %v", c, err)
		}
	}
	out, _ = run(t, t.TempDir(), "inspect", "--help")
	if !strings.Contains(out, "podman_sock_uri") {
		t.Error("inspect help should document the --json keys")
	}
}
