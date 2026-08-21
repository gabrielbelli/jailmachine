package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/doctor"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Host filesystem sharing on the command line (ADR 0007). A share is named
// by its host path alone: it appears in the guest at the same absolute
// path, so "-v /work/src:/app" and "-v /work/src:/work/src" both work and
// jm never has to rewrite an engine argument.

// mountFlagUsage is shared by "jm init" and "jm set".
const mountFlagUsage = "share a host directory with the guest at the same absolute path; append :ro for read-only (repeatable)"

// defaultMountRoots are the host roots a new machine shares unless
// --no-mounts is given: the user's home tree, where macOS mounts removable
// and network volumes, the real location of /tmp, and the per-user
// temporary directory $TMPDIR lives in. Roots that do not exist are
// skipped.
//
// $TMPDIR matters as much as the others: "mktemp -d", os.MkdirTemp and
// every test harness that builds a scratch directory return a path under
// /var/folders/<hash>, and without that root "-v $(mktemp -d):/app" would
// mount an empty guest directory with no error from jm or from podman
// (ADR 0007).
//
// /tmp itself is not here and cannot be: it is a symlink to /private/tmp,
// and a share mounted at the guest's own /tmp would shadow it. /private/tmp
// is shared instead, so a /tmp/... volume argument has to be written
// /private/tmp/... — README.md and "jm init --help" say so.
func defaultMountRoots() []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, home)
	}
	roots = append(roots, "/Volumes", "/private/tmp")
	if t := userTempRoot(); t != "" {
		roots = append(roots, t)
	}
	return roots
}

// userTempRoot is the per-user temporary directory tree $TMPDIR sits in
// (/var/folders/<hash>, the parent of its "T"), or "" when the host has no
// such layout.
func userTempRoot() string {
	tmp := filepath.Clean(os.TempDir())
	if !strings.HasPrefix(tmp, "/var/folders/") {
		return ""
	}
	root := filepath.Dir(tmp)
	if root == "/var/folders" || !strings.HasPrefix(root, "/var/folders/") {
		return ""
	}
	return root
}

// defaultShares builds the default share set, dropping roots the host does
// not have.
func defaultShares() ([]machine.Share, error) {
	var in []machine.Share
	for _, root := range defaultMountRoots() {
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			continue
		}
		in = append(in, machine.Share{HostPath: root})
	}
	return machine.NormaliseShares(in)
}

// parseMount turns a --mount value into a share. The value is a host path,
// optionally suffixed with ":ro" or ":rw"; a leading "~" is expanded so a
// quoted "~/code" behaves like the unquoted one.
func parseMount(v string) (machine.Share, error) {
	v = strings.TrimSpace(v)
	readOnly := false
	switch {
	case strings.HasSuffix(v, ":ro"):
		readOnly, v = true, strings.TrimSuffix(v, ":ro")
	case strings.HasSuffix(v, ":rw"):
		v = strings.TrimSuffix(v, ":rw")
	case strings.Contains(v, ":"):
		return machine.Share{}, usagef("--mount %q: a share is named by its host path alone (it appears in the guest at the same path); the only suffix is :ro", v)
	}
	p, err := expandTilde(v)
	if err != nil {
		return machine.Share{}, usage(err)
	}
	s, err := machine.NewShare(p, readOnly)
	if err != nil {
		return machine.Share{}, usage(err)
	}
	return s, nil
}

// expandTilde replaces a leading "~" or "~/" with the user's home
// directory.
func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// parseMounts parses every --mount value in order.
func parseMounts(values []string) ([]machine.Share, error) {
	var out []machine.Share
	for _, v := range values {
		s, err := parseMount(v)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// warnMissingShares prints one warning per share whose host path is not
// there, so that adding a path that does not exist yet (an unplugged disk)
// is visible without being fatal.
func warnMissingShares(shares []machine.Share) {
	_, skipped := machine.UsableShares(shares)
	for _, s := range skipped {
		fmt.Fprintf(stderr, "jm: warning: %s: %s; it is not shared until it is back\n", s.Share.HostPath, s.Reason)
	}
}

// warnUnsupportedShares warns when a machine asks for shares its backend
// cannot provide, rather than letting a mount fail inside a container
// (ADR 0007).
func warnUnsupportedShares(m *machine.Machine, b backend.Backend) {
	if len(m.Shares) == 0 || b.Capabilities().FileSharing {
		return
	}
	fmt.Fprintf(stderr, "jm: warning: backend %q cannot share host directories; %d share(s) are ignored\n", b.Name(), len(m.Shares))
}

// applyMounts folds --mount/--unmount values into a share set.
func applyMounts(list []machine.Share, add []string, remove []string) ([]machine.Share, error) {
	out := list
	for _, v := range remove {
		p, err := expandTilde(strings.TrimSuffix(strings.TrimSuffix(v, ":ro"), ":rw"))
		if err != nil {
			return nil, usage(err)
		}
		next, found, err := machine.RemoveShare(out, p)
		if err != nil {
			return nil, usage(err)
		}
		if !found {
			return nil, usagef("--unmount %s: the machine does not share that path (jm inspect lists the shares)", v)
		}
		out = next
	}
	shares, err := parseMounts(add)
	if err != nil {
		return nil, err
	}
	for _, s := range shares {
		if out, err = machine.AddShare(out, s); err != nil {
			return nil, usage(err)
		}
	}
	return machine.NormaliseShares(out)
}

// sameShares reports whether two share sets are identical, so that "jm set"
// only demands a restart when something really changed.
func sameShares(a, b []machine.Share) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The guest half of ADR 0007. Sharing only works when the guest mounts what
// the hypervisor attaches, and nothing else in jm notices when it does not:
// a machine created before the guest carried the jm_shares service accepts
// "jm set --mount /work", attaches the device on the next boot and mounts
// nothing, so "-v /work/x:/app" silently yields an empty directory.

// shareProbeTimeout bounds the guest-side share checks.
const shareProbeTimeout = 30 * time.Second

// warnMissingShareSupport warns, once per start, when a machine has shares
// but its guest has no service to mount them. It is a warning rather than a
// failure for the same reason a guest without jm_rtcsync is: the machine
// works, one feature does not (ADR 0007).
func warnMissingShareSupport(ctx context.Context, m *machine.Machine, client *sshx.Client) {
	if len(m.Shares) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, shareProbeTimeout)
	defer cancel()
	ok, err := client.FileExists(ctx, machine.GuestSharesRC)
	if err != nil || ok {
		return
	}
	fmt.Fprintf(stderr, "jm: warning: %s has no %s service, so its %d shared director%s attached to the VM but never mounted; re-create the machine to get one\n",
		m.Name, machine.GuestSharesService, len(m.Shares), plural(len(m.Shares), "y is", "ies are"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// firstWritableShare returns the first share that exists on the host and can
// be written to, which is the only kind the parity check can use.
func firstWritableShare(shares []machine.Share) (machine.Share, bool) {
	usable, _ := machine.UsableShares(shares)
	for _, s := range usable {
		if !s.ReadOnly {
			return s, true
		}
	}
	return machine.Share{}, false
}

// sharesParityCheck asserts the guarantee ADR 0007 makes and nothing else
// verifies: a file written on the host is visible inside the guest at the
// same absolute path. Statting the host path and asking the backend whether
// it can share host directories proves neither that the guest mounted
// anything nor that it mounted it where the volume argument will look.
//
// It stops at the guest rather than starting a container on purpose: podman
// bind-mounts the guest's own filesystem, so a directory the guest has at
// its identity path is the directory a container is given, and "jm doctor"
// must not pull an image to say so.
func sharesParityCheck(ctx context.Context, m *machine.Machine) (doctor.Result, bool) {
	res := doctor.Result{Name: "share parity " + m.Name}
	if len(m.Shares) == 0 {
		return res, false
	}
	if st, err := currentState(m); err != nil || st != backend.Running {
		return res, false
	}
	if b, err := backendFor(m); err != nil || !b.Capabilities().FileSharing {
		// checkMachineShares already reports a backend that cannot share.
		return res, false
	}
	share, ok := firstWritableShare(m.Shares)
	if !ok {
		return res, false
	}
	ctx, cancel := context.WithTimeout(ctx, shareProbeTimeout)
	defer cancel()
	ep, err := endpointOf(m)
	if err != nil {
		res.Status, res.Detail = doctor.Warn, err.Error()
		return res, true
	}
	client, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		res.Status, res.Detail = doctor.Warn, fmt.Sprintf("cannot reach the guest to check: %v", err)
		res.Fix = "jm start" + nameHint(m.Name)
		return res, true
	}
	defer client.Close()

	if ok, err := client.FileExists(ctx, machine.GuestSharesRC); err == nil && !ok {
		res.Status = doctor.Fail
		res.Detail = fmt.Sprintf("the guest has no %s service, so the %d attached share(s) are never mounted",
			machine.GuestSharesService, len(m.Shares))
		res.Fix = "the machine predates host directory sharing; re-create it with 'jm rm " + m.Name + "' and 'jm init'"
		return res, true
	}
	path, token, cleanup, err := writeShareSentinel(share.HostPath)
	if err != nil {
		res.Status, res.Detail = doctor.Warn, fmt.Sprintf("cannot write into %s to check: %v", share.HostPath, err)
		return res, true
	}
	defer cleanup()
	out, _, err := client.Run(ctx, "cat "+shellQuote(path))
	switch {
	case err != nil:
		res.Status = doctor.Fail
		res.Detail = fmt.Sprintf("a file written in %s is not in the guest at the same path", share.HostPath)
		res.Fix = fmt.Sprintf("mount them: 'jm ssh%s service %s start'; if that says the host share is not attached, 'jm stop%s && jm start%s'",
			nameHint(m.Name), machine.GuestSharesService, nameHint(m.Name), nameHint(m.Name))
	case strings.TrimSpace(out) != token:
		res.Status = doctor.Fail
		res.Detail = fmt.Sprintf("%s in the guest is a different directory from the host's", share.HostPath)
		res.Fix = fmt.Sprintf("something else is mounted there; 'jm ssh%s mount -t p9fs' shows what", nameHint(m.Name))
	default:
		res.Status = doctor.OK
		res.Detail = fmt.Sprintf("a file written in %s is in the guest at the same path", share.HostPath)
	}
	return res, true
}

// writeShareSentinel puts a file with unguessable contents in dir and
// returns its path, its contents and a cleanup function.
func writeShareSentinel(dir string) (path, token string, cleanup func(), err error) {
	f, err := os.CreateTemp(dir, ".jm-doctor-*")
	if err != nil {
		return "", "", func() {}, err
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		_ = f.Close()
		cleanup()
		return "", "", func() {}, err
	}
	token = hex.EncodeToString(buf[:])
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		cleanup()
		return "", "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return name, token, cleanup, nil
}
