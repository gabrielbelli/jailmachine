//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLifecycle drives the built ./jm binary through init, start, a podman
// run, stop, a warm start and rm. It needs qemu, podman and network access;
// run it with "make build && JM_E2E=1 make e2e".
func TestLifecycle(t *testing.T) {
	cfg := requireE2E(t)
	h := &harness{bin: cfg.bin, root: t.TempDir(), name: "e2e"}
	name := h.name
	jm := func(args ...string) string {
		t.Helper()
		return h.jm(t, args...)
	}
	// Converge on a clean slate whatever happens.
	// rm --force also takes the guest's containers and both podman
	// connections with it, under its own limit.
	t.Cleanup(func() {
		runCmdFor(t, h.command("rm", "--force", name), cleanupTimeout)
	})

	h.jmFor(t, provisionTimeout, "init", name, "--image", cfg.image, "--disk", strconv.Itoa(cfg.diskGiB), "--ssh-port", "2223")
	h.jmFor(t, provisionTimeout, "start", name)

	// podman writes image-pull progress to stderr; only stdout must be "hi".
	podmanHi := func(connection string) {
		t.Helper()
		r := runCmd(t, exec.Command("podman", "--connection", connection, "run", "--rm", "--os=linux", "docker.io/alpine", "echo", "hi"))
		if r.err != nil {
			t.Fatalf("podman --connection %s run: %v", connection, r.err)
		}
		if strings.TrimSpace(r.stdout) != "hi" {
			t.Fatalf("podman --connection %s run printed %q on stdout, want hi", connection, r.stdout)
		}
	}
	// Over SSH (the default connection) and over the provider's proxied
	// unix socket.
	podmanHi(name)
	podmanHi(name + "-sock")

	// The proxied socket answers the libpod API directly.
	insp := h.inspect(t)
	if insp.APISocket == "" {
		t.Fatal("inspect reports no api_socket")
	}
	ping := runCmd(t, exec.Command("curl", "-sf", "--max-time", "60", "--unix-socket", insp.APISocket, "http://d/v5.0.0/libpod/_ping"))
	if ping.err != nil || strings.TrimSpace(ping.stdout) != "OK" {
		t.Fatalf("libpod _ping over %s: %q, %v", insp.APISocket, ping.stdout, ping.err)
	}

	if out := jm("env", name); !strings.Contains(out, "DOCKER_HOST=") {
		t.Fatalf("jm env lacks DOCKER_HOST: %s", out)
	}

	// Port publishing (ADR 0004): a -p container becomes reachable on the
	// host through the forwarder, disappears when the container goes, and
	// comes back after a machine restart.
	waitCurl := func(url string, want bool, timeout time.Duration) {
		t.Helper()
		if !waitFor(timeout, 2*time.Second, func() bool { return curlOK(url) == want }) {
			t.Fatalf("curl %s reachable=%v still not true after %s\nports: %s", url, want, timeout, jm("ports", name))
		}
	}
	podman(t, name, append([]string{"run", "-d", "--name", "web", "-p", "8080:80"}, httpdArgs...)...)
	waitCurl("http://127.0.0.1:8080/", true, 90*time.Second)
	// Published on every host interface, as docker is on Linux: the same
	// port answers over IPv6 loopback, which is what "localhost" resolves
	// to first on macOS.
	waitCurl("http://[::1]:8080/", true, 30*time.Second)
	if out := jm("ports", name); !strings.Contains(out, "0.0.0.0:8080") {
		t.Fatalf("jm ports does not list 0.0.0.0:8080:\n%s", out)
	}
	podman(t, name, "rm", "-f", "web")
	waitCurl("http://127.0.0.1:8080/", false, 30*time.Second)

	podman(t, name, append([]string{"run", "-d", "--name", "web2", "-p", "8081:80"}, httpdArgs...)...)
	waitCurl("http://127.0.0.1:8081/", true, 90*time.Second)

	// Live disk grow: the hypervisor is told (QMP block_resize) and the guest
	// pool must actually be bigger afterwards, not just the record. The
	// margin below the new size allows for the partition table and ZFS's
	// own reservation.
	grown := cfg.diskGiB + 4
	jm("set", name, "--disk", strconv.Itoa(grown))
	pool := h.run(t, "ssh", name, "--", "zpool list -Hp -o size zroot")
	if n, err := strconv.ParseInt(strings.TrimSpace(pool.stdout), 10, 64); pool.err != nil || err != nil || n < int64(cfg.diskGiB+2)<<30 {
		t.Fatalf("zroot not grown to at least %d GiB after live set --disk %d: %q (%v)", cfg.diskGiB+2, grown, pool.stdout, pool.err)
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
	// podman restarts web2 with the guest (restart policy aside, the
	// forwarder must republish whatever is running after the warm start).
	podman(t, name, "start", "web2")
	waitCurl("http://127.0.0.1:8081/", true, 60*time.Second)
	podman(t, name, "rm", "-f", "web2")

	jm("rm", name)
	if _, err := os.Stat(h.dir()); !os.IsNotExist(err) {
		t.Fatalf("machine directory still present after rm")
	}
}
