package sleeper

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
)

// The stand-in (ADR 0009) holds a suspended machine's host endpoints. It is
// payload-blind: it reads no byte of a held connection before the upstream is
// dialled, never answers one, closes a connection it cannot hand over, and
// steps out of the path once the real endpoints are back.
const (
	// MaxHeld is how many connections are held at once; more are closed.
	MaxHeld = 256
	// ParkTimeout is how long one connection is held before it is closed.
	// It covers a wake that falls back to a cold boot.
	ParkTimeout = 120 * time.Second
)

// Kind is the endpoint a connection arrived on.
type Kind string

const (
	// Unix is the engine socket.
	Unix Kind = "unix"
	// TCP is the SSH port.
	TCP Kind = "tcp"
)

// Mode is what the sleeper is doing (ADR 0009 mode table).
type Mode string

const (
	// ModeMonitor: the machine is running and nothing is held.
	ModeMonitor Mode = "monitor"
	// ModeHold: endpoints are held (or must be taken, on a suspended
	// machine); connections wait for a wake.
	ModeHold Mode = "hold"
	// ModeHandOver: the real endpoints are back; held connections are
	// relayed and the stand-in steps out.
	ModeHandOver Mode = "hand-over"
	// ModeExit: the machine is stopped; the sleeper ends.
	ModeExit Mode = "exit"
)

// Observation is what one evaluation of the mode table sees.
type Observation struct {
	// State is the machine's combined state.
	State backend.State
	// Journal is whether a suspend journal exists.
	Journal bool
	// Holding is whether the stand-in holds an endpoint or a connection.
	Holding bool
	// EndpointsBack is Standin.EndpointsBack: the engine socket is a new
	// inode that answers, and the SSH port was released and answers.
	EndpointsBack bool
	// StoppedBefore is whether the previous evaluation saw a stopped
	// machine (no journal, no hypervisor).
	StoppedBefore bool
	// Busy is whether a command may be in the middle of a transition: the
	// machine lock is taken, or a waker (or the process the SSH port was
	// released to) is alive. A wake that falls back to a cold boot reads
	// stopped for a moment.
	Busy bool
}

// Stopped reports whether o counts as stopped for the exit rule.
func (o Observation) Stopped() bool { return o.State == backend.Stopped && !o.Journal }

// Decide is the mode table. A stopped machine seen twice in a row, with no
// command in the middle of a transition, ends the sleeper; held endpoints are
// handed over only when the journal is gone, the machine runs and the real
// endpoints answer, and are otherwise kept whatever the state (a broken
// machine is held, never suspended); a suspended machine whose endpoints are
// not held is held; a running one is monitored.
func Decide(o Observation) Mode {
	if o.Stopped() && o.StoppedBefore && !o.Busy {
		return ModeExit
	}
	if o.Holding {
		if !o.Journal && o.State == backend.Running && o.EndpointsBack {
			return ModeHandOver
		}
		return ModeHold
	}
	switch o.State {
	case backend.Suspended, backend.Broken:
		return ModeHold
	}
	return ModeMonitor
}

// held is one parked connection.
type held struct {
	conn  net.Conn
	kind  Kind
	at    time.Time
	timer *time.Timer
}

// listener is one taken endpoint and its accept loop.
type listener struct {
	ln   net.Listener
	done chan struct{}
}

// Standin holds endpoints and the connections that arrive on them.
type Standin struct {
	// MaxHeld and ParkTimeout default to the package constants when zero.
	MaxHeld     int
	ParkTimeout time.Duration
	// OnAccept is called, outside any lock, after a connection is parked.
	OnAccept func(Kind)

	// wrap, when set, wraps every accepted connection (tests count reads).
	wrap func(net.Conn) net.Conn

	mu         sync.Mutex
	unix       *listener
	unixPath   string
	unixIno    uint64
	unixAside  string // a hard link to the socket TakeUnix replaced
	tcp        *listener
	tcpAddr    string
	releasedTo int
	held       map[*held]struct{}
	armed      bool
	aborted    bool
}

func (s *Standin) maxHeld() int {
	if s.MaxHeld > 0 {
		return s.MaxHeld
	}
	return MaxHeld
}

func (s *Standin) parkTimeout() time.Duration {
	if s.ParkTimeout > 0 {
		return s.ParkTimeout
	}
	return ParkTimeout
}

// besidePath is the name a new socket is bound under before it is renamed
// over path: in the same directory (rename is atomic only there), shorter
// than any socket name jm uses, so it fits wherever path fits, and distinct
// per path, because a socket that fell back to the shared temp directory
// sits next to other machines' sockets.
func besidePath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(filepath.Dir(path), ".jmw"+hex.EncodeToString(sum[:2]))
}

// asidePath is where TakeUnixKeepingPrevious keeps a hard link to the socket
// it replaces, beside besidePath and just as short.
func asidePath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(filepath.Dir(path), ".jmo"+hex.EncodeToString(sum[:2]))
}

// inodeOf returns the inode of whatever is at path.
func inodeOf(path string) (uint64, bool) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(s.Ino), true
}

// TakeUnix takes the unix socket at path over without a moment where the
// path is missing: a new socket is bound beside it and renamed over it. The
// listener never unlinks the path when it closes, because by then the path
// may name the socket that took it back. Connections are parked, never read.
func (s *Standin) TakeUnix(path string) error { return s.takeUnix(path, false) }

// TakeUnixKeepingPrevious is TakeUnix that first keeps a hard link to the
// socket at path, if any. A listener is bound to its inode, not to its name,
// so RestorePrevious can put a socket that is still being served back
// without restarting what serves it. DropPrevious forgets it.
func (s *Standin) TakeUnixKeepingPrevious(path string) error { return s.takeUnix(path, true) }

func (s *Standin) takeUnix(path string, keep bool) error {
	tmp := besidePath(path)
	if len(tmp) > backend.MaxSocketPath {
		return fmt.Errorf("sleeper: socket path %q is too long", tmp)
	}
	_ = os.Remove(tmp)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return fmt.Errorf("sleeper: binding %s: %w", tmp, err)
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sleeper: %w", err)
	}
	ino, ok := inodeOf(tmp)
	if !ok {
		ln.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sleeper: %s vanished after binding", tmp)
	}
	s.DropPrevious()
	aside := ""
	if keep {
		a := asidePath(path)
		_ = os.Remove(a)
		if os.Link(path, a) == nil {
			aside = a
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		_ = os.Remove(tmp)
		if aside != "" {
			_ = os.Remove(aside)
		}
		return fmt.Errorf("sleeper: taking over %s: %w", path, err)
	}
	l := &listener{ln: ln, done: make(chan struct{})}
	s.mu.Lock()
	old := s.unix
	s.unix, s.unixPath, s.unixIno, s.unixAside = l, path, ino, aside
	s.mu.Unlock()
	if old != nil {
		old.ln.Close()
		<-old.done
	}
	go s.accept(l, Unix)
	return nil
}

// RestorePrevious puts the socket TakeUnixKeepingPrevious replaced back over
// the path, in one rename, when the path is still the stand-in's own and the
// previous socket still answers. It reports whether it did; either way the
// link is gone afterwards. The stand-in's listener stays open for HandOver.
func (s *Standin) RestorePrevious() bool {
	s.mu.Lock()
	path, ino, aside := s.unixPath, s.unixIno, s.unixAside
	s.unixAside = ""
	s.mu.Unlock()
	if aside == "" {
		return false
	}
	if cur, ok := inodeOf(path); !ok || cur != ino || !Answers(aside) {
		_ = os.Remove(aside)
		return false
	}
	if err := os.Rename(aside, path); err != nil {
		_ = os.Remove(aside)
		return false
	}
	return true
}

// DropPrevious removes the link TakeUnixKeepingPrevious kept, if any.
func (s *Standin) DropPrevious() {
	s.mu.Lock()
	aside := s.unixAside
	s.unixAside = ""
	s.mu.Unlock()
	if aside != "" {
		_ = os.Remove(aside)
	}
}

// TakeTCP listens on addr. It fails while anything else holds the port.
func (s *Standin) TakeTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sleeper: listening on %s: %w", addr, err)
	}
	l := &listener{ln: ln, done: make(chan struct{})}
	s.mu.Lock()
	old := s.tcp
	s.tcp, s.tcpAddr, s.releasedTo = l, addr, 0
	s.mu.Unlock()
	if old != nil {
		old.ln.Close()
		<-old.done
	}
	go s.accept(l, TCP)
	return nil
}

// CloseUnix stops holding the unix endpoint. With unlink, the path is removed
// too, but only while it is still the stand-in's own socket: a socket that
// replaced it belongs to someone else. Held connections stay held.
func (s *Standin) CloseUnix(unlink bool) error {
	s.DropPrevious()
	s.mu.Lock()
	l, path, ino := s.unix, s.unixPath, s.unixIno
	s.unix, s.unixPath, s.unixIno = nil, "", 0
	s.mu.Unlock()
	if l != nil {
		l.ln.Close()
		<-l.done
	}
	if unlink && path != "" {
		if cur, ok := inodeOf(path); ok && cur == ino {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// CloseTCP stops holding the SSH port altogether. Held connections stay held.
func (s *Standin) CloseTCP() {
	s.mu.Lock()
	l := s.tcp
	s.tcp, s.tcpAddr, s.releasedTo = nil, "", 0
	s.mu.Unlock()
	if l != nil {
		l.ln.Close()
		<-l.done
	}
}

// ReleaseTCP closes the TCP listener for a waker with pid, which is about to
// start the network provider on that port. Held TCP connections are kept, and
// the endpoint still counts as held until it is handed over or taken again.
// The pid is recorded even when the port is not held, so the port is not
// taken in the gap before that waker binds it.
func (s *Standin) ReleaseTCP(pid int) error {
	s.mu.Lock()
	l := s.tcp
	s.tcp, s.releasedTo = nil, pid
	s.mu.Unlock()
	if l != nil {
		l.ln.Close()
		<-l.done
	}
	return nil
}

// RelistenIfOwnerGone takes the SSH port back when the waker it was released
// to has died. It reports whether it listens again.
func (s *Standin) RelistenIfOwnerGone(alive func(pid int) bool) (bool, error) {
	s.mu.Lock()
	pid, addr, listening := s.releasedTo, s.tcpAddr, s.tcp != nil
	s.mu.Unlock()
	if pid == 0 || listening || addr == "" || alive(pid) {
		return false, nil
	}
	if err := s.TakeTCP(addr); err != nil {
		return false, err
	}
	return true, nil
}

// ReleasedTo is the pid the SSH port was released to, 0 when it was not.
func (s *Standin) ReleasedTo() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releasedTo
}

// UnixPath is the unix endpoint held, "" when none.
func (s *Standin) UnixPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unixPath
}

// UnixIsOurs reports whether the unix endpoint is held and its path is still
// the stand-in's own socket.
func (s *Standin) UnixIsOurs() bool {
	s.mu.Lock()
	path, ino := s.unixPath, s.unixIno
	s.mu.Unlock()
	if path == "" {
		return false
	}
	cur, ok := inodeOf(path)
	return ok && cur == ino
}

// TCPAddr is the SSH endpoint held (listening or released), "" when none.
func (s *Standin) TCPAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcpAddr
}

// TCPListening reports whether the stand-in listens on the SSH port now.
func (s *Standin) TCPListening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcp != nil
}

// Holding reports whether an endpoint is held or a connection is parked.
func (s *Standin) Holding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unixPath != "" || s.tcpAddr != "" || len(s.held) > 0
}

// Held is the number of parked connections.
func (s *Standin) Held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

// FirstKind is the endpoint of the longest-held connection, "" when none.
func (s *Standin) FirstKind() Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first *held
	for h := range s.held {
		if first == nil || h.at.Before(first.at) {
			first = h
		}
	}
	if first == nil {
		return ""
	}
	return first.kind
}

// EndpointsBack reports whether the real endpoints serve again: the unix path
// is a socket that is not the stand-in's and answers, and the SSH port was
// released and answers. An endpoint that is not held does not count against.
func (s *Standin) EndpointsBack() bool {
	s.mu.Lock()
	path, ino, addr, listening := s.unixPath, s.unixIno, s.tcpAddr, s.tcp != nil
	s.mu.Unlock()
	if path != "" {
		cur, ok := inodeOf(path)
		if !ok || cur == ino || !Answers(path) {
			return false
		}
	}
	if addr != "" {
		if listening {
			return false
		}
		c, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err != nil {
			return false
		}
		c.Close()
	}
	return true
}

// Arm marks the start of a suspend: from now until CheckAndFreeze, every
// parked connection and every Abort cancels it.
func (s *Standin) Arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.aborted = true, false
}

// Disarm ends the window Arm opened.
func (s *Standin) Disarm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = false
}

// Abort cancels an armed suspend; it does nothing otherwise.
func (s *Standin) Abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed {
		s.aborted = true
	}
}

// Aborted reports whether the armed suspend was cancelled.
func (s *Standin) Aborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

// CheckAndFreeze is the last abort check before the guest is frozen: it
// reports a cancellation, or disarms, atomically, so a connection that
// arrives afterwards waits for a wake instead of being lost between the two.
func (s *Standin) CheckAndFreeze() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return true
	}
	s.armed = false
	return false
}

// accept parks connections from l until it is closed.
func (s *Standin) accept(l *listener, kind Kind) {
	defer close(l.done)
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// EMFILE and friends: back off rather than spin.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if s.wrap != nil {
			c = s.wrap(c)
		}
		s.park(c, kind)
	}
}

func (s *Standin) park(c net.Conn, kind Kind) {
	s.mu.Lock()
	if s.held == nil {
		s.held = map[*held]struct{}{}
	}
	if len(s.held) >= s.maxHeld() {
		s.mu.Unlock()
		c.Close()
		return
	}
	h := &held{conn: c, kind: kind, at: time.Now()}
	h.timer = time.AfterFunc(s.parkTimeout(), func() {
		s.mu.Lock()
		_, still := s.held[h]
		delete(s.held, h)
		s.mu.Unlock()
		if still {
			c.Close()
		}
	})
	s.held[h] = struct{}{}
	if s.armed {
		s.aborted = true
	}
	cb := s.OnAccept
	s.mu.Unlock()
	if cb != nil {
		cb(kind)
	}
}

// take removes every parked connection from the queue and returns them.
func (s *Standin) take() []*held {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*held, 0, len(s.held))
	for h := range s.held {
		h.timer.Stop()
		out = append(out, h)
	}
	s.held = nil
	return out
}

// HandOver steps out of the path: both listeners close (the unix path is not
// unlinked; it names the real socket now), and every parked connection is
// relayed to a fresh upstream from the dialler for its kind. A connection
// whose upstream cannot be dialled is closed. It returns how many
// connections were handed over.
func (s *Standin) HandOver(dialUnix, dialTCP func() (net.Conn, error)) int {
	s.DropPrevious()
	s.mu.Lock()
	u, t := s.unix, s.tcp
	s.unix, s.unixPath, s.unixIno = nil, "", 0
	s.tcp, s.tcpAddr, s.releasedTo = nil, "", 0
	s.mu.Unlock()
	for _, l := range []*listener{u, t} {
		if l != nil {
			l.ln.Close()
			<-l.done // a connection accepted meanwhile is parked, then taken
		}
	}
	hs := s.take()
	for _, h := range hs {
		dial := dialTCP
		if h.kind == Unix {
			dial = dialUnix
		}
		go Splice(h.conn, dial)
	}
	return len(hs)
}

// CloseHeld closes every parked connection (its client sees EOF) and returns
// how many there were.
func (s *Standin) CloseHeld() int {
	hs := s.take()
	for _, h := range hs {
		h.conn.Close()
	}
	return len(hs)
}

// Close stops holding anything: parked connections are closed, both
// listeners closed, and the unix path unlinked only while it is still the
// stand-in's own socket.
func (s *Standin) Close() error {
	s.CloseHeld()
	s.CloseTCP()
	return s.CloseUnix(true)
}

// closeWriter is a connection that can end one direction.
type closeWriter interface{ CloseWrite() error }

func closeWrite(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// Splice dials an upstream for c and copies bytes both ways until both
// directions end. An end of file in one direction is passed on as a
// half-close, so a client that shuts down its write side still gets its
// answer. Nothing is read from c before the dial succeeds; a failed dial
// closes c.
func Splice(c net.Conn, dial func() (net.Conn, error)) {
	up, err := dial()
	if err != nil {
		c.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, c)
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, up)
		closeWrite(c)
	}()
	wg.Wait()
	c.Close()
	up.Close()
}
