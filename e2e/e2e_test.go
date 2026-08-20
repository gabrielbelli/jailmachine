//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLifecycle drives the built ./jm binary through init, start, a podman
// run, stop, a warm start and rm. It needs qemu, podman and network access;
// run it with "make build && JM_E2E=1 make e2e".
func TestLifecycle(t *testing.T) {
	if os.Getenv("JM_E2E") != "1" {
		t.Skip("set JM_E2E=1 to run the end-to-end test")
	}
	bin, err := filepath.Abs("../jm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s not built (run make build): %v", bin, err)
	}
	root := t.TempDir()
	const name = "e2e"

	jm := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"--state-root", root}, args...)...)
		out, err := cmd.CombinedOutput()
		t.Logf("$ jm %s\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("jm %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}
	// Converge on a clean slate whatever happens.
	t.Cleanup(func() {
		cmd := exec.Command(bin, "--state-root", root, "rm", "--force", name)
		out, _ := cmd.CombinedOutput()
		t.Logf("cleanup: %s", out)
	})

	jm("init", name, "--image", "official:15.1-RELEASE", "--ssh-port", "2223")
	jm("start", name)

	// podman writes image-pull progress to stderr; only stdout must be "hi".
	run := exec.Command("podman", "--connection", name, "run", "--rm", "--os=linux", "docker.io/alpine", "echo", "hi")
	var podmanErr bytes.Buffer
	run.Stderr = &podmanErr
	out, err := run.Output()
	t.Logf("podman run stdout: %s\nstderr: %s", out, podmanErr.String())
	if err != nil {
		t.Fatalf("podman run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("podman run printed %q on stdout, want hi", out)
	}

	jm("stop", name)
	if out := jm("inspect", name); !strings.Contains(out, "stopped") {
		t.Fatalf("expected stopped after stop: %s", out)
	}

	began := time.Now()
	jm("start", name)
	if d := time.Since(began); d > 60*time.Second {
		t.Errorf("warm start took %s, want < 60s", d)
	}
	if out := jm("list"); !strings.Contains(out, name) || !strings.Contains(out, "running") {
		t.Fatalf("list does not show %s running: %s", name, out)
	}

	jm("rm", name)
	if _, err := os.Stat(filepath.Join(root, "machines", name)); !os.IsNotExist(err) {
		t.Fatalf("machine directory still present after rm")
	}
}
