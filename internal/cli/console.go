package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// followPoll is how often the follower re-checks the log for new bytes.
const followPoll = 250 * time.Millisecond

func newConsoleCmd() *cobra.Command {
	var (
		follow bool
		lines  int
	)
	cmd := &cobra.Command{
		Use:   "console [name]",
		Short: "Show the guest's serial console log",
		Long:  "Print the last lines of the guest's serial console log, as written by the hypervisor. With -f keep printing new output until Ctrl-C.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			b, err := backendFor(m)
			if err != nil {
				return err
			}
			path, err := consolePath(m, b)
			if err != nil {
				return err
			}
			return showConsole(cmd.Context(), stdout, path, lines, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new console output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of trailing lines to print")
	return cmd
}

// consolePath returns the backend's console log, or a clear error when the
// backend has none.
func consolePath(m *machine.Machine, b backend.Backend) (string, error) {
	if !b.Capabilities().SerialConsole {
		return "", fmt.Errorf("backend %q has no serial console", b.Name())
	}
	path := b.ConsolePath(m)
	if path == "" {
		return "", fmt.Errorf("backend %q did not provide a console log path", b.Name())
	}
	return path, nil
}

// showConsole prints the last n lines of path and, if follow is set, keeps
// streaming appended bytes (tail -f) until ctx is cancelled. A log that
// does not exist yet is an error unless following, in which case it is
// waited for.
func showConsole(ctx context.Context, w io.Writer, path string, n int, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !follow {
			return fmt.Errorf("no console log yet at %s (has the machine been started?)", path)
		}
		fmt.Fprintf(w, "waiting for %s\n", path)
		if f, err = waitForFile(ctx, path); err != nil {
			return err
		}
		if f == nil {
			return nil // cancelled before the log appeared
		}
	}
	defer f.Close()

	tail, off, err := lastLines(f, n)
	if err != nil {
		return err
	}
	if _, err := w.Write(tail); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	return followFile(ctx, w, f, off)
}

// waitForFile polls until path can be opened or ctx ends; a cancelled ctx
// returns (nil, nil).
func waitForFile(ctx context.Context, path string) (*os.File, error) {
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(followPoll):
		}
	}
}

// lastLines returns the last n lines of f (all of it when it has fewer) and
// the offset just past the data returned, reading the file backwards in
// chunks so a large log is not loaded whole. n <= 0 returns nothing.
func lastLines(f *os.File, n int) ([]byte, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if n <= 0 || size == 0 {
		return nil, size, nil
	}
	const chunk = 64 << 10
	var (
		buf   []byte
		end   = size
		found = 0
	)
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		part := make([]byte, end-start)
		if _, err := f.ReadAt(part, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		buf = append(part, buf...)
		end = start
		// Count newlines; a trailing newline terminates the last line rather
		// than starting an empty one.
		found = 0
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' && i != len(buf)-1 {
				found++
				if found == n {
					return buf[i+1:], size, nil
				}
			}
		}
	}
	return buf, size, nil
}

// followFile streams bytes appended to f after off until ctx ends. A file
// that shrinks (truncated by a restart) is read again from the start.
func followFile(ctx context.Context, w io.Writer, f *os.File, off int64) error {
	buf := make([]byte, 32<<10)
	for {
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if st.Size() < off {
			off = 0
		}
		for st.Size() > off {
			k, err := f.ReadAt(buf, off)
			if k > 0 {
				if _, werr := w.Write(buf[:k]); werr != nil {
					return werr
				}
				off += int64(k)
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followPoll):
		}
	}
}
