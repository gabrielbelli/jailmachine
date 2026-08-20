package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// Exit codes. Usage errors (bad flags, bad names, ambiguous defaults) get
// 2 so scripts can tell "you called it wrong" from "it failed".
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// usageError marks an error as the caller's fault (exit 2).
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usage wraps err as a usage error; nil stays nil.
func usage(err error) error {
	if err == nil {
		return nil
	}
	return &usageError{err: err}
}

// usagef formats a usage error.
func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// hintError attaches a "what to read or do next" hint to an error that is
// not tied to a start stage.
type hintError struct {
	err  error
	hint string
}

func (e *hintError) Error() string { return e.err.Error() }
func (e *hintError) Unwrap() error { return e.err }

// withHint attaches hint to err; nil stays nil.
func withHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &hintError{err: err, hint: hint}
}

// exitCode maps an error to the process exit status.
func exitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case isUsage(err):
		return ExitUsage
	}
	return ExitFailure
}

// isUsage reports whether err is a usage error: explicitly marked, or one
// of the validation sentinels from internal/machine.
func isUsage(err error) bool {
	var ue *usageError
	return errors.As(err, &ue) ||
		errors.Is(err, machine.ErrInvalidName) ||
		errors.Is(err, machine.ErrInvalidImage)
}

// formatError renders err for stderr as
//
//	jm: <command> <name>: <stage>: <cause>
//	  hint: <what to read or do>
//
// Empty parts are left out ("jm: list: ..." when no machine is involved,
// no stage line for errors outside jm start's stages). A StageError's
// hint and a hintError's hint become the second line.
func formatError(command, name string, err error) string {
	if err == nil {
		return ""
	}
	var (
		stage string
		hint  string
		cause = err
	)
	var se *machine.StageError
	if errors.As(err, &se) {
		stage = string(se.Stage)
		hint = se.Hint
		cause = se.Err
	}
	var he *hintError
	if hint == "" && errors.As(err, &he) {
		hint = he.hint
		cause = he.err
	}
	if isUsage(err) && hint == "" {
		hint = "run 'jm --help'"
		if command != "" {
			hint = "run 'jm " + command + " --help'"
		}
	}

	var b strings.Builder
	b.WriteString("jm: ")
	if command != "" {
		b.WriteString(command)
		if name != "" {
			b.WriteString(" " + name)
		}
		b.WriteString(": ")
	}
	if stage != "" {
		b.WriteString(stage + ": ")
	}
	b.WriteString(strings.TrimSpace(cause.Error()))
	if hint != "" {
		b.WriteString("\n  hint: " + hint)
	}
	return b.String()
}
