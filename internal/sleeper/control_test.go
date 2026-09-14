package sleeper

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlProtocol(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, ControlFile)
	// A socket left by a killed sleeper is replaced.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()

	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("control socket mode = %v, %v", st.Mode().Perm(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	go Serve(ctx, ln, func(_ context.Context, line string) string {
		switch f := Fields(line); f[0] {
		case "abort":
			return "ok"
		case "release-tcp":
			if len(f) != 2 {
				return "err release-tcp needs a pid"
			}
			return "ok"
		case "slow":
			<-release
			return "ok\nsecond line"
		}
		return "err unknown request"
	})

	req := func(line string) string {
		t.Helper()
		rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer rcancel()
		reply, err := Request(rctx, sock, line)
		if err != nil {
			t.Fatalf("Request(%q): %v", line, err)
		}
		return reply
	}
	if got := req("abort"); got != "ok" {
		t.Errorf("abort = %q", got)
	}
	if got := req("release-tcp"); got != "err release-tcp needs a pid" {
		t.Errorf("release-tcp = %q", got)
	}
	if got := req("frobnicate"); got != "err unknown request" {
		t.Errorf("unknown = %q", got)
	}
	if w, rest := SplitReply("busy 1 engine client connected"); w != "busy" || rest != "1 engine client connected" {
		t.Errorf("SplitReply = %q, %q", w, rest)
	}

	// A slow request does not hold up another, and a caller's deadline ends
	// its own wait.
	sctx, scancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = Request(sctx, sock, "slow")
	scancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("slow request with a deadline = %v", err)
	}
	if got := req("abort"); got != "ok" {
		t.Errorf("abort during a slow request = %q", got)
	}
	close(release)

	if _, err := Listen(sock); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("Listen on a live control socket = %v", err)
	}
	if _, err := Request(context.Background(), filepath.Join(dir, "none.sock"), "abort"); err == nil {
		t.Error("Request to no sleeper succeeded")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sleeper.json")
	at := time.Date(2026, 9, 14, 14, 2, 0, 0, time.UTC)
	want := Status{PID: 42, Mode: ModeHold, State: "suspended", SuspendedAt: &at, LastSuspendMS: 9213, LastWakeBy: "socket", SSHWake: true}
	if err := WriteStatus(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 42 || got.Mode != ModeHold || got.SuspendedAt == nil || !got.SuspendedAt.Equal(at) ||
		got.LastSuspendMS != 9213 || got.LastWakeBy != "socket" || !got.SSHWake {
		t.Errorf("round trip = %+v", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

// Serve returns only once every handler that started has replied, so the
// sleeper never closes its stand-in under a request in flight.
func TestServeWaitsForHandlers(t *testing.T) {
	sock := filepath.Join(shortDir(t), ControlFile)
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(ctx, ln, func(context.Context, string) string {
			close(entered)
			<-release
			return "ok"
		})
	}()
	replies := make(chan string, 1)
	go func() {
		reply, _ := Request(context.Background(), sock, "slow")
		replies <- reply
	}()
	<-entered
	cancel()
	select {
	case <-served:
		t.Fatal("Serve returned with a handler still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its handler finished")
	}
	if got := <-replies; got != "ok" {
		t.Errorf("reply = %q", got)
	}
}
