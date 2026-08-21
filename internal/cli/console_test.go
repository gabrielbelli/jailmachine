package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func TestLastLines(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) *os.File {
		p := filepath.Join(dir, "log")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc\n", 10, "a\nb\nc\n"},
		{"a\nb\nc", 1, "c"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\nc\n", 0, ""},
		{"", 5, ""},
		{"single", 5, "single"},
	}
	for _, tc := range cases {
		f := write(tc.in)
		got, off, err := lastLines(f, tc.n)
		if err != nil || string(got) != tc.want || off != int64(len(tc.in)) {
			t.Errorf("lastLines(%q, %d) = %q, %d, %v; want %q, %d", tc.in, tc.n, got, off, err, tc.want, len(tc.in))
		}
	}
	// Larger than one read chunk.
	big := strings.Repeat("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde\n", 3000)
	f := write(big)
	got, _, err := lastLines(f, 3)
	if err != nil || strings.Count(string(got), "\n") != 3 {
		t.Errorf("big tail: %d lines, %v", strings.Count(string(got), "\n"), err)
	}
}

func TestConsoleCommand(t *testing.T) {
	root := t.TempDir()
	m := seedRecord(t, root, "alpha")
	log := machine.NewStore(root).Path(m.Name, machine.ConsoleFile)

	if _, err := run(t, root, "console", "alpha"); err == nil || !strings.Contains(err.Error(), "no console log yet") {
		t.Errorf("missing log: %v", err)
	}
	if err := os.WriteFile(log, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "console", "alpha", "-n", "2")
	if err != nil || out != "two\nthree\n" {
		t.Errorf("console -n 2 = %q, %v", out, err)
	}
}

func TestShowConsoleFollow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(p, []byte("boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var buf safeBuffer
	done := make(chan error, 1)
	go func() { done <- showConsole(ctx, &buf, p, 50, true) }()

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("login:\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "login:") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "boot\nlogin:\n" {
		t.Errorf("followed output = %q", got)
	}
}

func TestShowConsoleFollowCancelledBeforeLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "console.log") // never created
	ctx, cancel := context.WithCancel(context.Background())
	var buf safeBuffer
	done := make(chan error, 1)
	go func() { done <- showConsole(ctx, &buf, p, 50, true) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel before the log exists: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("showConsole did not return after cancel")
	}
	if !strings.Contains(buf.String(), "waiting for") {
		t.Errorf("output = %q", buf.String())
	}
}

// safeBuffer is a bytes.Buffer safe for a writer and a polling reader. The
// mutex is zero-value ready: lazily creating it would itself be the race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
