// Package sshx wraps golang.org/x/crypto/ssh for the control channel
// (ADR 0001): key generation, wait-until-reachable, run a command, and an
// interactive shell via the system ssh binary.
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// ConnectTimeout bounds the TCP connect and SSH handshake of a single Dial.
const ConnectTimeout = 3 * time.Second

// PollInterval is the delay between WaitReady attempts.
const PollInterval = 2 * time.Second

// Client is a connected SSH session factory for the guest.
type Client struct {
	conn *ssh.Client
}

// Dial connects to host:port as user, authenticating with the private key
// at keyPath. Host key verification is deliberately disabled: the guest VM
// is created by us, its sshd host key is regenerated on every fresh image,
// and it is only reachable through a loopback port forward, so pinning a
// host key would add friction without any security benefit (the PoC's ssh
// options did the same with StrictHostKeyChecking=no).
func Dial(ctx context.Context, host string, port int, user, keyPath string) (*Client, error) {
	signer, err := loadSigner(keyPath)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see doc comment
		Timeout:         ConnectTimeout,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	dialCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	var d net.Dialer
	tcp, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshx: dial %s: %w", addr, err)
	}
	// The handshake does not take a context; enforce the timeout via a
	// deadline on the raw connection and clear it afterwards.
	_ = tcp.SetDeadline(time.Now().Add(ConnectTimeout))
	c, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		tcp.Close()
		return nil, fmt.Errorf("sshx: handshake %s: %w", addr, err)
	}
	_ = tcp.SetDeadline(time.Time{})
	return &Client{conn: ssh.NewClient(c, chans, reqs)}, nil
}

// Run executes cmd on the guest and returns its captured stdout and stderr.
// A non-zero exit status is returned as an *ssh.ExitError wrapped in err.
// If ctx is cancelled mid-command the session is closed and ctx.Err() is
// returned.
func (c *Client) Run(ctx context.Context, cmd string) (stdout, stderr string, err error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return "", "", fmt.Errorf("sshx: new session: %w", err)
	}
	defer sess.Close()

	var out, errb bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errb

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		<-done
		return out.String(), errb.String(), ctx.Err()
	case err = <-done:
		if err != nil {
			err = fmt.Errorf("sshx: run %q: %w", cmd, err)
		}
		return out.String(), errb.String(), err
	}
}

// RunScript is Run for a multi-line program. A non-zero exit is reported by
// status alone: Run names the command it ran, which for a generated script
// means the whole program ends up in the error message and buries what
// actually went wrong. Callers pass what the script was for and print its
// captured output themselves.
func (c *Client) RunScript(ctx context.Context, what, script string) (stdout, stderr string, err error) {
	out, errOut, err := c.Run(ctx, script)
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		err = fmt.Errorf("sshx: %s exited %d", what, exit.ExitStatus())
	}
	return out, errOut, err
}

// FileExists reports whether path is a regular file on the guest, using
// "test -f". Only a transport failure is an error; a missing file is
// (false, nil).
func (c *Client) FileExists(ctx context.Context, path string) (bool, error) {
	return c.test(ctx, "-f", path)
}

// SocketExists is FileExists for unix sockets ("test -S").
func (c *Client) SocketExists(ctx context.Context, path string) (bool, error) {
	return c.test(ctx, "-S", path)
}

func (c *Client) test(ctx context.Context, flag, path string) (bool, error) {
	return c.Succeeds(ctx, "test "+flag+" "+shellQuote(path))
}

// Succeeds runs cmd and reports whether it exited zero. A non-zero exit is
// an answer, not an error; only a transport failure is returned as one.
func (c *Client) Succeeds(ctx context.Context, cmd string) (bool, error) {
	_, _, err := c.Run(ctx, cmd)
	if err == nil {
		return true, nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, err
}

// Close tears down the underlying connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// WaitReady dials repeatedly, every PollInterval, until a connection
// succeeds or ctx is done. onAttempt (may be nil) is called before each
// attempt with a 1-based counter, so callers can print progress dots.
func WaitReady(ctx context.Context, host string, port int, user, keyPath string, onAttempt func(attempt int)) (*Client, error) {
	var lastErr error
	for attempt := 1; ; attempt++ {
		if onAttempt != nil {
			onAttempt(attempt)
		}
		c, err := Dial(ctx, host, port, user, keyPath)
		if err == nil {
			return c, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("sshx: not reachable after %d attempts: %w (last error: %v)", attempt, ctx.Err(), lastErr)
		case <-time.After(PollInterval):
		}
	}
}

// Args returns the argument vector (excluding argv[0]) used by Interactive,
// mirroring the PoC's ssh_opts. Exposed so callers can reuse it for
// "podman system connection" hints and so it can be unit-tested.
func Args(host string, port int, user, keyPath string, args []string) []string {
	a := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=" + strconv.Itoa(int(ConnectTimeout/time.Second)),
		"-p", strconv.Itoa(port),
		user + "@" + host,
	}
	return append(a, args...)
}

// ForwardArgs returns the argument vector (excluding argv[0]) for a
// detached "ssh -N" that serves localSock on the host and forwards every
// connection to remoteSock in the guest. The stale local socket is
// unlinked first and the process exits if the forward cannot be set up, so
// a supervisor can tell success from failure by the socket appearing.
func ForwardArgs(host string, port int, user, keyPath, localSock, remoteSock string) []string {
	base := Args(host, port, user, keyPath, nil)
	dest := base[len(base)-1]
	opts := []string{
		"-N", "-n", "-T",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StreamLocalBindUnlink=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-L", localSock + ":" + remoteSock,
	}
	// Options must precede the destination; anything after it is a
	// remote command.
	return append(append(base[:len(base)-1:len(base)-1], opts...), dest)
}

// Interactive replaces the current process with the system ssh binary,
// attached to the terminal, for "jm ssh [cmd...]". It only returns on
// failure to exec.
func Interactive(host string, port int, user, keyPath string, args []string) error {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("sshx: ssh binary not found: %w", err)
	}
	argv := append([]string{bin}, Args(host, port, user, keyPath, args)...)
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		return fmt.Errorf("sshx: exec %s: %w", bin, err)
	}
	return nil
}

// GenerateKey writes a fresh ed25519 private key in OpenSSH format (0600)
// to path and the matching authorized_keys line to path+".pub" (0644).
// The parent directory is created with 0700 if missing.
func GenerateKey(path string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("sshx: generate key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "jailmachine")
	if err != nil {
		return fmt.Errorf("sshx: marshal key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("sshx: public key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("sshx: mkdir: %w", err)
	}
	// pem.EncodeToMemory output is "-----BEGIN OPENSSH PRIVATE KEY-----\n...".
	if err := os.WriteFile(path, pemBytes(block), 0o600); err != nil {
		return fmt.Errorf("sshx: write %s: %w", path, err)
	}
	pubLine := bytes.TrimSpace(ssh.MarshalAuthorizedKey(sshPub))
	pubLine = append(pubLine, " jailmachine\n"...)
	if err := os.WriteFile(path+".pub", pubLine, 0o644); err != nil {
		return fmt.Errorf("sshx: write %s.pub: %w", path, err)
	}
	return nil
}

// ForgetKnownHost removes any stale entry for [host]:port from the user's
// ~/.ssh/known_hosts by running "ssh-keygen -R". podman-remote trusts that
// file, so a rebuilt VM with a new host key would otherwise be rejected.
// Output is discarded; a missing ssh-keygen or missing entry is not an
// error.
func ForgetKnownHost(host string, port int) error {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return nil
	}
	cmd := exec.Command(bin, "-R", KnownHostName(host, port))
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
	return nil
}

// KnownHostName is the known_hosts key for host:port as OpenSSH writes it:
// "[host]:port" for non-standard ports, the bare host on port 22.
func KnownHostName(host string, port int) string {
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}

func loadSigner(keyPath string) (ssh.Signer, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("sshx: read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("sshx: parse key %s: %w", keyPath, err)
	}
	return signer, nil
}
