package cli

import (
	"fmt"
	"io"
	"os"
)

// quiet is the persistent -q/--quiet flag: stage lines, notes and progress
// are suppressed; data output (list, inspect, env, ports) and errors are
// not.
var quiet bool

// Quiet reports whether -q/--quiet was requested.
func Quiet() bool { return quiet }

// isTerminal reports whether w is an interactive terminal. Progress bars
// and spinners are only drawn on one; a pipe or a file gets plain lines.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// showProgress reports whether progress bars may be drawn: an interactive
// stdout, and neither --quiet nor --json.
func showProgress() bool {
	return !quiet && !jsonOut && isTerminal(stdout)
}

// progressWriter is what long-running stages (image fetch) write their
// human-readable output to. It carries whether animated progress is wanted
// so the producer can fall back to plain lines (internal/image checks for
// the Interactive method).
type progressWriter struct {
	io.Writer
	interactive bool
}

// Interactive reports whether bars and spinners may be drawn.
func (p progressWriter) Interactive() bool { return p.interactive }

// progressOut returns the writer to hand to a Fetch-like stage: quiet
// discards everything, otherwise stdout with the TTY decision attached.
func progressOut() io.Writer {
	if quiet {
		return io.Discard
	}
	return progressWriter{Writer: stdout, interactive: showProgress()}
}

// dot prints one progress dot for a polling loop, unless quiet.
func dot() {
	if quiet {
		return
	}
	fmt.Fprint(stdout, ".")
}

// endDots terminates a line of dots, unless quiet (when none were printed).
func endDots() {
	if quiet {
		return
	}
	fmt.Fprintln(stdout)
}
