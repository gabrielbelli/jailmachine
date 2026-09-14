package sleeper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// The control protocol on sleeper.sock is one request line per connection and
// one reply line (ADR 0009):
//
//	abort                 ok
//	release-tcp <pid>     ok | err <msg>
//	woke [<by> [<ms>]]    ok
//	suspend [force]       ok <ms> <allocated bytes> | busy <reason> |
//	                      unavailable <reason> | err <msg>
//	status                the JSON of sleeper.json
//
// Anything else is "err unknown request".

// dialTimeout bounds a dial that only checks whether something answers.
const dialTimeout = 100 * time.Millisecond

// maxLine bounds a request or reply line.
const maxLine = 1 << 16

// Answers reports whether a unix dial to path succeeds.
func Answers(path string) bool {
	c, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Listen creates the control socket at path, mode 0600. A socket nothing
// answers on is litter from a killed sleeper and is replaced; one that
// answers belongs to a live sleeper and is an error.
func Listen(path string) (net.Listener, error) {
	if Answers(path) {
		return nil, ErrAlreadyRunning
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("sleeper: removing a stale %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("sleeper: listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("sleeper: %w", err)
	}
	return ln, nil
}

// Handler answers one request line with one reply line.
type Handler func(ctx context.Context, line string) string

// Serve answers requests on ln until ctx is done or ln is closed. Requests
// are handled concurrently; a slow one (a suspend) does not hold up another
// (an abort). A request still being read when ctx ends is dropped, and Serve
// returns only once every handler that started has replied.
func Serve(ctx context.Context, ln net.Listener, handle Handler) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			stopRead := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()) })
			line, err := readLine(bufio.NewReaderSize(c, 4096))
			if !stopRead() || err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			reply := handle(ctx, line)
			_, _ = c.Write([]byte(oneLine(reply) + "\n"))
		}()
	}
}

// Request sends one line to the control socket at sock and returns the reply
// line. ctx bounds the whole exchange.
func Request(ctx context.Context, sock, line string) (string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if _, err := c.Write([]byte(oneLine(line) + "\n")); err != nil {
		return "", err
	}
	reply, err := readLine(bufio.NewReaderSize(c, 4096))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// The connection's deadline is the context's, and can fire
			// a moment before the context notices.
			return "", context.DeadlineExceeded
		}
		return "", fmt.Errorf("sleeper: reading the reply to %q: %w", Fields(line)[0], err)
	}
	return reply, nil
}

// Fields splits a request line into words; an empty line is one empty word.
func Fields(line string) []string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return []string{""}
	}
	return f
}

// SplitReply splits a reply into its first word and the rest.
func SplitReply(reply string) (word, rest string) {
	word, rest, _ = strings.Cut(strings.TrimSpace(reply), " ")
	return word, rest
}

func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadString('\n')
		b.WriteString(chunk)
		if b.Len() > maxLine {
			return "", errors.New("sleeper: line too long")
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if b.Len() > 0 && strings.HasSuffix(b.String(), "\n") {
				break
			}
			return "", err
		}
		break
	}
	return strings.TrimRight(b.String(), "\r\n"), nil
}

// oneLine keeps a line to one line.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
