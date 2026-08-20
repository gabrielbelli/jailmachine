//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
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
	podmanHi := func(connection string) {
		t.Helper()
		run := exec.Command("podman", "--connection", connection, "run", "--rm", "--os=linux", "docker.io/alpine", "echo", "hi")
		var podmanErr bytes.Buffer
		run.Stderr = &podmanErr
		out, err := run.Output()
		t.Logf("podman --connection %s run stdout: %s\nstderr: %s", connection, out, podmanErr.String())
		if err != nil {
			t.Fatalf("podman --connection %s run: %v", connection, err)
		}
		if strings.TrimSpace(string(out)) != "hi" {
			t.Fatalf("podman --connection %s run printed %q on stdout, want hi", connection, out)
		}
	}
	// Over SSH (the default connection) and over the provider's proxied
	// unix socket.
	podmanHi(name)
	podmanHi(name + "-sock")

	// The proxied socket answers the libpod API directly.
	var insp struct {
		APISocket string `json:"api_socket"`
	}
	if err := json.Unmarshal([]byte(jm("--json", "inspect", name)), &insp); err != nil {
		t.Fatal(err)
	}
	if insp.APISocket == "" {
		t.Fatal("inspect reports no api_socket")
	}
	ping := exec.Command("curl", "-sf", "--unix-socket", insp.APISocket, "http://d/v5.0.0/libpod/_ping")
	out, err := ping.CombinedOutput()
	t.Logf("curl _ping: %s", out)
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("libpod _ping over %s: %q, %v", insp.APISocket, out, err)
	}

	if out := jm("env", name); !strings.Contains(out, "DOCKER_HOST=") {
		t.Fatalf("jm env lacks DOCKER_HOST: %s", out)
	}

	// Port publishing (ADR 0004): a -p container becomes reachable on the
	// host through the forwarder, disappears when the container goes, and
	// comes back after a machine restart.
	podman := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("podman", append([]string{"--connection", name}, args...)...)
		out, err := cmd.CombinedOutput()
		t.Logf("$ podman --connection %s %s\n%s", name, strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("podman %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}
	curlOK := func(url string) bool {
		out, err := exec.Command("curl", "-fsS", "--max-time", "3", url).Output()
		return err == nil && strings.TrimSpace(string(out)) == "ok"
	}
	waitCurl := func(url string, want bool, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if curlOK(url) == want {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("curl %s reachable=%v still not true after %s\nports: %s", url, want, timeout, jm("ports", name))
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "--connection", name, "rm", "-f", "web", "web2").Run()
	})
	// busybox httpd rather than nginx: nginx's workers need Linux AIO
	// (io_setup), which the Linuxulator does not implement, so nginx
	// accepts connections but never answers them.
	httpd := []string{"--os=linux", "docker.io/busybox", "sh", "-c",
		"mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 80 -h /www"}
	podman(append([]string{"run", "-d", "--name", "web", "-p", "8080:80"}, httpd...)...)
	waitCurl("http://127.0.0.1:8080/", true, 90*time.Second)
	if out := jm("ports", name); !strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("jm ports does not list 127.0.0.1:8080:\n%s", out)
	}
	podman("rm", "-f", "web")
	waitCurl("http://127.0.0.1:8080/", false, 30*time.Second)

	podman(append([]string{"run", "-d", "--name", "web2", "-p", "8081:80"}, httpd...)...)
	waitCurl("http://127.0.0.1:8081/", true, 90*time.Second)

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
	// podman restarts web2 with the guest (restart policy aside, the
	// forwarder must republish whatever is running after the warm start).
	podman("start", "web2")
	waitCurl("http://127.0.0.1:8081/", true, 60*time.Second)
	podman("rm", "-f", "web2")

	jm("rm", name)
	if _, err := os.Stat(filepath.Join(root, "machines", name)); !os.IsNotExist(err) {
		t.Fatalf("machine directory still present after rm")
	}
}
