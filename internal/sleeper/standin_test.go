package sleeper

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
)

// shortDir is a directory short enough for unix socket paths.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serveUnix listens at path and closes every connection it accepts.
func serveUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// replaceBeside binds a new socket beside path and renames it over, as the
// engine socket forward does, and returns it.
func replaceBeside(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	tmp := path + ".new"
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestTakeUnixNoMissingWindow(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	serveUnix(t, path)

	var stop atomic.Bool
	var missing, dials atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c, err := net.DialTimeout("unix", path, time.Second)
			if err != nil {
				if errors.Is(err, syscall.ENOENT) {
					missing.Add(1)
				}
				continue
			}
			dials.Add(1)
			c.Close()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	s := &Standin{MaxHeld: 1 << 20}
	if err := s.TakeUnix(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if !s.UnixIsOurs() {
		t.Error("the path is not the stand-in's socket after TakeUnix")
	}
	replaceBeside(t, path)
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	s.Close()

	if n := missing.Load(); n != 0 {
		t.Errorf("%d dials found the socket path missing", n)
	}
	if dials.Load() == 0 {
		t.Error("no dial succeeded")
	}
	if _, err := os.Stat(besidePath(path)); !os.IsNotExist(err) {
		t.Errorf("the temporary name was left behind: %v", err)
	}
	if len(filepath.Base(besidePath(path))) >= len("podman.sock") {
		t.Errorf("beside name %q is not shorter than the socket name", filepath.Base(besidePath(path)))
	}
}

// readCounter counts Read calls on a connection.
type readCounter struct {
	net.Conn
	reads *atomic.Int64
}

func (r readCounter) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.Conn.Read(p)
}

func (r readCounter) CloseWrite() error { return r.Conn.(closeWriter).CloseWrite() }

// echoUpstream serves one reply per connection: everything read until EOF,
// prefixed. got receives what each connection sent.
func echoUpstream(t *testing.T, network, addr string) (net.Listener, chan []byte) {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan []byte, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				got <- data
				_, _ = c.Write(append([]byte("reply:"), data[:min(len(data), 16)]...))
			}()
		}
	}()
	return ln, got
}

func TestStandinNeverReadsBeforeHandOver(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	var reads atomic.Int64
	s := &Standin{}
	s.wrap = func(c net.Conn) net.Conn { return readCounter{Conn: c, reads: &reads} }
	if err := s.TakeUnix(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /_ping HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	c.(*net.UnixConn).CloseWrite()
	waitFor(t, "a held connection", func() bool { return s.Held() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := reads.Load(); n != 0 {
		t.Fatalf("%d reads while holding", n)
	}

	upPath := filepath.Join(dir, "up.sock")
	_, got := echoUpstream(t, "unix", upPath)
	readsAtDial := int64(-1)
	n := s.HandOver(func() (net.Conn, error) {
		readsAtDial = reads.Load()
		return net.Dial("unix", upPath)
	}, nil)
	if n != 1 {
		t.Fatalf("handed over %d connections", n)
	}
	reply, _ := io.ReadAll(c)
	if readsAtDial != 0 {
		t.Errorf("%d reads before the upstream dial", readsAtDial)
	}
	if string(<-got) != "GET /_ping HTTP/1.1\r\n\r\n" || string(reply) != "reply:GET /_ping HTTP/" {
		t.Errorf("reply = %q", reply)
	}
}

func TestHandOverSplicesExactBytesHalfClose(t *testing.T) {
	payload := make([]byte, 300_000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []Kind{Unix, TCP} {
		t.Run(string(kind), func(t *testing.T) {
			dir := shortDir(t)
			s := &Standin{}
			defer s.Close()
			var endpoint, upNet, upAddr string
			if kind == Unix {
				endpoint, upNet, upAddr = filepath.Join(dir, "podman.sock"), "unix", filepath.Join(dir, "up.sock")
				if err := s.TakeUnix(endpoint); err != nil {
					t.Fatal(err)
				}
			} else {
				endpoint, upNet, upAddr = freeTCPAddr(t), "tcp", freeTCPAddr(t)
				if err := s.TakeTCP(endpoint); err != nil {
					t.Fatal(err)
				}
			}
			c, err := net.Dial(map[Kind]string{Unix: "unix", TCP: "tcp"}[kind], endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			wrote := make(chan error, 1)
			go func() {
				_, err := c.Write(payload)
				if err == nil {
					err = c.(interface{ CloseWrite() error }).CloseWrite()
				}
				wrote <- err
			}()
			waitFor(t, "a held connection", func() bool { return s.Held() == 1 })
			_, got := echoUpstream(t, upNet, upAddr)
			dial := func() (net.Conn, error) { return net.Dial(upNet, upAddr) }
			fail := func() (net.Conn, error) { return nil, errors.New("wrong endpoint kind") }
			if kind == Unix {
				s.HandOver(dial, fail)
			} else {
				s.HandOver(fail, dial)
			}
			reply, err := io.ReadAll(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-wrote; err != nil {
				t.Fatal(err)
			}
			if data := <-got; !bytes.Equal(data, payload) {
				t.Errorf("upstream got %d bytes, want the exact %d", len(data), len(payload))
			}
			if want := append([]byte("reply:"), payload[:16]...); !bytes.Equal(reply, want) {
				t.Errorf("client got %q after its half-close", reply)
			}
			if s.Holding() {
				t.Error("still holding after the hand-over")
			}
		})
	}
}

func TestHandOverRequiresJournalGoneRunningAndNewInode(t *testing.T) {
	for _, c := range []struct {
		o    Observation
		want Mode
	}{
		{Observation{State: backend.Broken, Holding: true, EndpointsBack: true}, ModeHold},
		{Observation{State: backend.Suspended, Journal: true, Holding: true, EndpointsBack: true}, ModeHold},
		{Observation{State: backend.Running, Journal: true, Holding: true, EndpointsBack: true}, ModeHold},
		{Observation{State: backend.Running, Holding: true}, ModeHold},
		{Observation{State: backend.Running, Holding: true, EndpointsBack: true}, ModeHandOver},
		{Observation{State: backend.Suspended, Journal: true}, ModeHold},
		{Observation{State: backend.Broken}, ModeHold},
		{Observation{State: backend.Running}, ModeMonitor},
		{Observation{State: backend.Running, Journal: true}, ModeMonitor},
		{Observation{State: backend.Stopped}, ModeMonitor},
		{Observation{State: backend.Stopped, Holding: true}, ModeHold},
		{Observation{State: backend.Stopped, Holding: true, StoppedBefore: true}, ModeExit},
		{Observation{State: backend.Stopped, StoppedBefore: true}, ModeExit},
		{Observation{State: backend.Stopped, StoppedBefore: true, Busy: true}, ModeMonitor},
		{Observation{State: backend.Stopped, Holding: true, StoppedBefore: true, Busy: true}, ModeHold},
	} {
		if got := Decide(c.o); got != c.want {
			t.Errorf("Decide(%+v) = %s, want %s", c.o, got, c.want)
		}
	}

	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	s := &Standin{}
	defer s.Close()
	if err := s.TakeUnix(path); err != nil {
		t.Fatal(err)
	}
	if s.EndpointsBack() {
		t.Error("back while the inode is the stand-in's own")
	}
	// A replacement nothing answers on is not back.
	dead := replaceBeside(t, path)
	dead.Close()
	if s.EndpointsBack() {
		t.Error("back while the new socket does not answer")
	}
	replaceBeside(t, path)
	if !s.EndpointsBack() {
		t.Error("not back with a new inode that answers")
	}

	addr := freeTCPAddr(t)
	if err := s.TakeTCP(addr); err != nil {
		t.Fatal(err)
	}
	if s.EndpointsBack() {
		t.Error("back while the stand-in still listens on the SSH port")
	}
	if err := s.ReleaseTCP(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if s.EndpointsBack() {
		t.Error("back while nothing answers on the released port")
	}
	echoUpstream(t, "tcp", addr)
	if !s.EndpointsBack() {
		t.Error("not back with the port released and answering")
	}
}

func TestCloseUnixDoesNotUnlinkReplacedSocket(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	s := &Standin{}
	if err := s.TakeUnix(path); err != nil {
		t.Fatal(err)
	}
	replaceBeside(t, path)
	want, _ := inodeOf(path)
	if err := s.CloseUnix(true); err != nil {
		t.Fatal(err)
	}
	if got, ok := inodeOf(path); !ok || got != want {
		t.Errorf("the replacing socket was removed or changed")
	}

	path2 := filepath.Join(dir, "other.sock")
	if err := s.TakeUnix(path2); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseUnix(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path2); err != nil {
		t.Errorf("CloseUnix(false) removed the stand-in's own socket: %v", err)
	}
	if err := s.TakeUnix(path2); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseUnix(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path2); !os.IsNotExist(err) {
		t.Errorf("CloseUnix(true) kept the stand-in's own socket: %v", err)
	}
}

func TestHeldLimitAndTimeout(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	accepted := make(chan Kind, 8)
	s := &Standin{MaxHeld: 2, ParkTimeout: 200 * time.Millisecond, OnAccept: func(k Kind) { accepted <- k }}
	if err := s.TakeUnix(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var conns []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
		<-accepted
	}
	over, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := over.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("a connection over the limit was not closed: %v", err)
	}
	if n := s.Held(); n != 2 {
		t.Errorf("held = %d", n)
	}
	waitFor(t, "the park timeout", func() bool { return s.Held() == 0 })
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("a timed-out connection was not closed: %v", err)
		}
	}
}

func TestAbortOnlyWhileArmed(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	s := &Standin{}
	if err := s.TakeUnix(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Abort()
	if s.Aborted() {
		t.Error("an abort while not armed counted")
	}
	s.Arm()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "a held connection", func() bool { return s.Held() == 1 })
	if !s.Aborted() || !s.CheckAndFreeze() {
		t.Error("a connection while armed did not abort")
	}
	s.Arm()
	if s.Aborted() || s.CheckAndFreeze() {
		t.Error("Arm did not reset the abort")
	}
	c2, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitFor(t, "a second held connection", func() bool { return s.Held() == 2 })
	if s.Aborted() {
		t.Error("a connection after the freeze aborted")
	}
}

func TestReleaseTCPAndRelistenWhenWakerDies(t *testing.T) {
	addr := freeTCPAddr(t)
	s := &Standin{}
	defer s.Close()
	if err := s.TakeTCP(addr); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "a held connection", func() bool { return s.Held() == 1 })
	if err := s.ReleaseTCP(4242); err != nil {
		t.Fatal(err)
	}
	if s.TCPListening() || s.Held() != 1 || s.ReleasedTo() != 4242 || !s.Holding() {
		t.Fatalf("after release: listening %v, held %d, released to %d", s.TCPListening(), s.Held(), s.ReleasedTo())
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is not free after the release: %v", err)
	}
	ln.Close()
	if ok, err := s.RelistenIfOwnerGone(func(int) bool { return true }); ok || err != nil {
		t.Errorf("relistened while the waker lives: %v, %v", ok, err)
	}
	if ok, err := s.RelistenIfOwnerGone(func(int) bool { return false }); !ok || err != nil {
		t.Fatalf("did not relisten after the waker died: %v, %v", ok, err)
	}
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitFor(t, "a second held connection", func() bool { return s.Held() == 2 })
	if s.ReleasedTo() != 0 {
		t.Error("the release survived the relisten")
	}

	// A release when the port is not held (S17 could not take it) still
	// names the waker, so the port is not taken in the gap before it binds.
	s2 := &Standin{}
	if err := s2.ReleaseTCP(4343); err != nil {
		t.Fatal(err)
	}
	if s2.ReleasedTo() != 4343 || s2.Holding() {
		t.Errorf("release without a held port: released to %d, holding %v", s2.ReleasedTo(), s2.Holding())
	}
}

// A rollback before the forward is stopped puts the forward's own socket back:
// its listener is bound to the inode, so the old name serves again with the
// connections it carries. A previous socket nothing serves is not restored.
func TestRestorePreviousGivesBackServedSocket(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "podman.sock")
	prev, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	prev.SetUnlinkOnClose(false)
	defer prev.Close()
	go func() {
		for {
			c, err := prev.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("forward"))
			c.Close()
		}
	}()
	prevIno, _ := inodeOf(path)
	s := &Standin{}
	defer s.Close()
	if err := s.TakeUnixKeepingPrevious(path); err != nil {
		t.Fatal(err)
	}
	if !s.UnixIsOurs() {
		t.Fatal("the stand-in does not hold the path")
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "a held connection", func() bool { return s.Held() == 1 })
	if !s.RestorePrevious() {
		t.Fatal("a served previous socket was not restored")
	}
	if cur, _ := inodeOf(path); cur != prevIno {
		t.Fatal("the path is not the previous socket")
	}
	c2, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(c2); string(got) != "forward" {
		t.Errorf("restored socket answered %q", got)
	}
	c2.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("litter beside the socket: %v", entries)
	}
	if s.RestorePrevious() {
		t.Error("restored twice")
	}

	// A previous socket nothing serves stays aside and is dropped.
	prev.Close()
	path2 := filepath.Join(dir, "other.sock")
	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: path2, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	dead.Close()
	if err := s.TakeUnixKeepingPrevious(path2); err != nil {
		t.Fatal(err)
	}
	if s.RestorePrevious() || !s.UnixIsOurs() {
		t.Error("restored a socket nothing serves")
	}
	if err := s.TakeUnixKeepingPrevious(path2); err != nil {
		t.Fatal(err)
	}
	s.DropPrevious()
	if err := s.CloseUnix(true); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("litter after DropPrevious: %v", entries)
	}
}
