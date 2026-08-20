package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestGenerateKey(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "ssh", "id_ed25519")
	if err := GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %o, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(key)
	if !strings.HasPrefix(string(raw), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("private key is not OpenSSH format: %q", raw[:40])
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	pubRaw, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey(pubRaw)
	if err != nil {
		t.Fatalf("parse pub line %q: %v", pubRaw, err)
	}
	if comment != "jailmachine" {
		t.Errorf("comment = %q, want jailmachine", comment)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %s", pub.Type())
	}
	if string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("public key does not match private key")
	}
}

func TestArgs(t *testing.T) {
	got := strings.Join(Args("127.0.0.1", 2222, "root", "/k", []string{"uname", "-a"}), " ")
	want := "-i /k -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=3 -p 2222 root@127.0.0.1 uname -a"
	if got != want {
		t.Errorf("Args = %q\nwant %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// startServer runs a minimal SSH server on loopback that accepts the given
// authorised key and handles "exec" requests for a tiny fake shell:
// "echo ..." prints its arguments, "test -f <path>" checks the local
// filesystem, anything else exits 127.
func startServer(t *testing.T, authorised ssh.PublicKey) (host string, port int) {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorised.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(c, cfg)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func serveConn(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				var p struct{ Cmd string }
				_ = ssh.Unmarshal(r.Payload, &p)
				r.Reply(true, nil)
				status := fakeShell(ch, p.Cmd)
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}

func fakeShell(w io.ReadWriter, cmd string) uint32 {
	f := strings.Fields(cmd)
	switch {
	case len(f) > 0 && f[0] == "echo":
		fmt.Fprintln(w, strings.Join(f[1:], " "))
		return 0
	case len(f) == 3 && f[0] == "test" && (f[1] == "-f" || f[1] == "-S"):
		st, err := os.Stat(strings.Trim(f[2], "'"))
		if err != nil {
			return 1
		}
		if (f[1] == "-f" && st.Mode().IsRegular()) || (f[1] == "-S" && st.Mode()&os.ModeSocket != 0) {
			return 0
		}
		return 1
	case len(f) > 0 && f[0] == "fail":
		fmt.Fprintln(w.(ssh.Channel).Stderr(), "boom")
		return 3
	}
	return 127
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if err := GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	pubRaw, _ := os.ReadFile(key + ".pub")
	pub, _, _, _, err := ssh.ParseAuthorizedKey(pubRaw)
	if err != nil {
		t.Fatal(err)
	}
	host, port := startServer(t, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, host, port, "root", key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRun(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	out, errOut, err := c.Run(ctx, "echo hello world")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world\n" || errOut != "" {
		t.Errorf("Run = %q / %q", out, errOut)
	}

	_, errOut, err = c.Run(ctx, "fail")
	var exit *ssh.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Errorf("expected exit status 3, got %v", err)
	}
	if errOut != "boom\n" {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestFileExists(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	present := filepath.Join(dir, "marker")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := c.FileExists(ctx, present)
	if err != nil || !ok {
		t.Errorf("FileExists(present) = %v, %v", ok, err)
	}
	ok, err = c.FileExists(ctx, filepath.Join(dir, "absent"))
	if err != nil || ok {
		t.Errorf("FileExists(absent) = %v, %v", ok, err)
	}
}

func TestSocketExists(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ok, err := c.SocketExists(ctx, sock)
	if err != nil || !ok {
		t.Errorf("SocketExists(sock) = %v, %v", ok, err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = c.SocketExists(ctx, plain)
	if err != nil || ok {
		t.Errorf("SocketExists(regular file) = %v, %v", ok, err)
	}
}

func TestForwardArgs(t *testing.T) {
	got := strings.Join(ForwardArgs("127.0.0.1", 2222, "root", "/k", "/h/podman.sock", "/var/run/podman/podman.sock"), " ")
	for _, want := range []string{"-i /k", "-p 2222", " root@127.0.0.1", " -N ", "-o ExitOnForwardFailure=yes", "-o StreamLocalBindUnlink=yes", "-L /h/podman.sock:/var/run/podman/podman.sock"} {
		if !strings.Contains(got, want) {
			t.Errorf("ForwardArgs missing %q: %s", want, got)
		}
	}
	// Options must precede the destination, or ssh treats them as a
	// remote command.
	if strings.Index(got, "root@127.0.0.1") < strings.Index(got, "-L ") {
		t.Errorf("options after destination: %s", got)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	if err := GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	// Reserve a port and close it so nothing listens there.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	attempts := 0
	_, err := WaitReady(ctx, "127.0.0.1", port, "root", key, func(int) { attempts++ })
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts < 1 {
		t.Error("onAttempt never called")
	}
}

func TestKnownHostName(t *testing.T) {
	if got := KnownHostName("127.0.0.1", 22); got != "127.0.0.1" {
		t.Errorf("port 22: %q", got)
	}
	if got := KnownHostName("127.0.0.1", 2222); got != "[127.0.0.1]:2222" {
		t.Errorf("port 2222: %q", got)
	}
}

func TestForgetKnownHostNoBinary(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	t.Setenv("HOME", t.TempDir()) // isolate from the real known_hosts
	if err := ForgetKnownHost("127.0.0.1", 65000); err != nil {
		t.Error(err)
	}
}
