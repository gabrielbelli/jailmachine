package machine

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Stage names the steps of "jm start" (ADR 0005). Every failure names the
// stage that failed and the log to read.
type Stage string

const (
	// StageBackend boots the hypervisor.
	StageBackend Stage = "backend"
	// StageSSH waits until sshd in the guest answers.
	StageSSH Stage = "ssh"
	// StageProvision waits for the ready marker written by provision.sh.
	StageProvision Stage = "provision"
	// StageConnect registers the podman connection on the host.
	StageConnect Stage = "connect"
)

// StageError wraps an error with the stage it happened in and a hint about
// which log to read.
type StageError struct {
	Stage Stage
	Hint  string
	Err   error
}

func (e *StageError) Error() string {
	msg := fmt.Sprintf("stage %q failed: %v", e.Stage, e.Err)
	if e.Hint != "" {
		msg += " (" + e.Hint + ")"
	}
	return msg
}

func (e *StageError) Unwrap() error { return e.Err }

// NewStageError builds a StageError.
func NewStageError(stage Stage, hint string, err error) error {
	return &StageError{Stage: stage, Hint: hint, Err: err}
}

// SSHHost is the host-side address the guest's sshd is reachable on with
// user-mode (slirp) networking.
const SSHHost = "127.0.0.1"

// SSHEndpoint returns "host:port" for the machine's forwarded SSH port.
func (m *Machine) SSHEndpoint() string {
	return fmt.Sprintf("%s:%d", SSHHost, m.SSHPort)
}

// PodmanURI is the connection URI registered with
// "podman system connection add".
func (m *Machine) PodmanURI() string {
	return fmt.Sprintf("ssh://%s@%s:%d%s", m.SSHUser, SSHHost, m.SSHPort, GuestPodmanSocket)
}

// cliName is the stricter name grammar accepted on the command line:
// lower-case letters, digits and dashes, starting with a letter or digit.
var cliName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ErrInvalidName is wrapped by ValidateCLIName failures.
var ErrInvalidName = errors.New("invalid machine name")

// ValidateCLIName enforces the user-facing name grammar
// ([a-z0-9][a-z0-9-]*) on top of ValidateName.
func ValidateCLIName(name string) error {
	if !cliName.MatchString(name) {
		return fmt.Errorf("%w: %q (use [a-z0-9][a-z0-9-]*)", ErrInvalidName, name)
	}
	return ValidateName(name)
}

// ResolveName returns the machine name from an optional positional argument,
// falling back to DefaultName, and validates it.
func ResolveName(args []string) (string, error) {
	name := DefaultName
	if len(args) > 0 && args[0] != "" {
		name = args[0]
	}
	if err := ValidateCLIName(name); err != nil {
		return "", err
	}
	return name, nil
}

// ImageRef is a parsed "--image" value such as "official" or
// "official:15.1-RELEASE".
type ImageRef struct {
	// Source is the image provider name ("official").
	Source string
	// Release is the provider-specific version; empty means the provider's
	// default.
	Release string
}

// ErrInvalidImage is wrapped by ParseImageRef failures.
var ErrInvalidImage = errors.New("invalid image reference")

// ParseImageRef splits "<source>[:<release>]". An empty ref means the
// default image.
func ParseImageRef(ref string) (ImageRef, error) {
	if ref == "" {
		ref = DefaultImage
	}
	src, rel, _ := strings.Cut(ref, ":")
	if src == "" {
		return ImageRef{}, fmt.Errorf("%w: %q", ErrInvalidImage, ref)
	}
	if strings.ContainsAny(rel, "/ \t\n") {
		return ImageRef{}, fmt.Errorf("%w: %q", ErrInvalidImage, ref)
	}
	return ImageRef{Source: src, Release: rel}, nil
}

// String renders the reference back in "<source>[:<release>]" form.
func (r ImageRef) String() string {
	if r.Release == "" {
		return r.Source
	}
	return r.Source + ":" + r.Release
}
