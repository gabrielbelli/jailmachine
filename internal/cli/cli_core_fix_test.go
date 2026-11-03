package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// useRoot points the store at root for a test that calls the CLI's
// functions directly rather than through run.
func useRoot(t *testing.T, root string) {
	t.Helper()
	old := stateRoot
	stateRoot = root
	t.Cleanup(func() { stateRoot = old })
}

// The client-only wrapper calls ("jpodman --version") need no machine.
func TestWrapperTargetClientOnlyWithoutMachine(t *testing.T) {
	useRoot(t, t.TempDir())
	t.Setenv(MachineEnv, "")
	name, rest, err := wrapperTarget(context.Background(), []string{"--version"})
	if err != nil || name != "" || len(rest) != 1 || rest[0] != "--version" {
		t.Errorf("--version = %q, %v, %v", name, rest, err)
	}
	if _, _, err := wrapperTarget(context.Background(), []string{"ps"}); err == nil {
		t.Error("ps with no machine should fail")
	}
}

func TestResolveDefaultHintForDocker(t *testing.T) {
	old := activeCommand
	activeCommand = "docker"
	defer func() { activeCommand = old }()
	_, _, err := resolveDefault([]string{"dev", "dev2"})
	if err == nil || !strings.Contains(err.Error(), "JM_MACHINE=dev "+DockerWrapperName) {
		t.Errorf("docker hint = %v", err)
	}
}

// An unreadable default record is reported, not hidden behind "no
// machines exist" while "jm init" says the machine exists.
func TestResolveNameUnreadableDefault(t *testing.T) {
	root := t.TempDir()
	useRoot(t, root)
	m := seedRecord(t, root, machine.DefaultName)
	if err := os.WriteFile(machine.NewStore(root).Path(m.Name, machine.RecordFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveName(nil)
	if err == nil || strings.Contains(err.Error(), "no machines exist") || !strings.Contains(hintOf(err), "jm rm --force") {
		t.Errorf("err = %v (hint %q)", err, hintOf(err))
	}
}

func hintOf(err error) string {
	var h *hintError
	if errors.As(err, &h) {
		return h.hint
	}
	return ""
}

func TestHelperPath(t *testing.T) {
	dir := t.TempDir()
	jm := filepath.Join(dir, "jm")
	if err := os.WriteFile(jm, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{WrapperName, DockerWrapperName} {
		if got := helperPath(filepath.Join(dir, w)); got != jm {
			t.Errorf("%s -> %q, want %q", w, got, jm)
		}
	}
	// jm itself is not resolved through its symlink: the target may be a
	// versioned directory an upgrade deletes.
	if got := helperPath(jm); got != jm {
		t.Errorf("jm -> %q", got)
	}
	lone := filepath.Join(t.TempDir(), WrapperName)
	if got := helperPath(lone); got != lone {
		t.Errorf("wrapper without jm -> %q", got)
	}
}

func TestPortInUse(t *testing.T) {
	err := errors.New("gvproxy: exited before creating net.sock: listen tcp 127.0.0.1:2222: bind: address already in use")
	port, ok := portInUse(err)
	if !ok || port != "2222" {
		t.Fatalf("portInUse = %q, %v", port, ok)
	}
	if _, ok := portInUse(errors.New("gvproxy: exited")); ok {
		t.Error("no port in a plain error")
	}
}

// "jm init --memory" takes the units "jm set --memory" does.
func TestInitMemoryUnits(t *testing.T) {
	_, err := run(t, t.TempDir(), "init", "--memory", "4GiB", "--cpus", "0")
	if err == nil || !strings.Contains(err.Error(), "--cpus") {
		t.Errorf("4GiB should parse and fail on --cpus: %v", err)
	}
	_, err = run(t, t.TempDir(), "init", "--memory", "lots")
	if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--memory") {
		t.Errorf("junk memory = %v", err)
	}
}

// Start removes and re-adds the podman connection; a machine that was the
// default connection stays the default.
func TestStartKeepsDefaultConnection(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "run")
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	startFakeGuest(t, root, "run", defaultGuest)
	calls := isolateHostTools(t)
	// The fake podman answers "connection list" with "run" as the default.
	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(filepath.Dir(podmanPath), "calls.log")
	script := "#!/bin/sh\necho \"podman $*\" >> '" + logPath + "'\n" +
		"case \"$*\" in *'connection list'*) echo '[{\"Name\":\"run\",\"Default\":true}]';; esac\nexit 0\n"
	if err := os.WriteFile(podmanPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, root, "start", "run"); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	c := calls()
	rm := strings.Index(c, "podman system connection remove run")
	def := strings.Index(c, "podman system connection default run")
	if rm < 0 || def < rm {
		t.Errorf("the default connection was not restored after the re-add: %q", c)
	}
}
