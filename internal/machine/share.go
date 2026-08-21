package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Host filesystem sharing (ADR 0007). A Share offers one host directory to
// the guest at the *same* absolute path, so that a volume argument written
// on the host ("-v /work/src:/app", or "-v /work/src:/work/src") resolves
// inside the guest with nothing rewriting it.
//
// The share set lives on the Machine record because the guest's mount
// declarations must survive restarts: the backend exports each share under
// its Tag, and the guest learns which Tag belongs at which path from the
// share table the backend writes into GuestConfDir.

// MaxShares caps how many directories one machine exports. Backends pin a
// device address per share (ADR 0007), so the ceiling is the address space
// they set aside, not a policy.
const MaxShares = 16

// MaxTagLen is the longest mount tag the 9p protocol carries (32 bytes
// including the terminating NUL).
const MaxTagLen = 31

// shareTagPrefix marks a tag as ours in guest-side diagnostics.
const shareTagPrefix = "jm"

// Fixed names for the share table the host hands to the guest. The table
// is itself delivered as a share (GuestConfTag) so that the guest can
// mount everything declaratively at boot, without jm pushing a script over
// SSH at runtime (ADR 0003, ADR 0007).
const (
	// GuestConfDir is the directory inside a machine directory that is
	// exported read-only under GuestConfTag.
	GuestConfDir = "guest"
	// SharesTabFile is the share table inside GuestConfDir.
	SharesTabFile = "shares.tab"
	// GuestConfTag is the mount tag of the configuration share.
	GuestConfTag = "jmconf"
	// GuestConfMount is where the guest mounts GuestConfTag.
	GuestConfMount = "/var/db/jm/conf"
	// GuestSharesTab is the share table as the guest sees it.
	GuestSharesTab = GuestConfMount + "/" + SharesTabFile
	// GuestSharesRC is the rc script that mounts the shares in the guest
	// (guest/provision.sh installs it). A guest without it attaches the
	// devices and mounts nothing, which is silent: "-v /work/x:/app" then
	// yields an empty directory. Its absence is what "jm start" warns
	// about and "jm doctor" reports.
	GuestSharesRC = "/usr/local/etc/rc.d/jm_shares"
	// GuestSharesService is that script's service name.
	GuestSharesService = "jm_shares"
)

// ErrInvalidShare is wrapped by every share validation failure.
var ErrInvalidShare = errors.New("invalid share")

// Share is one host directory exported to the guest. GuestPath always
// equals HostPath (the identity-path rule of ADR 0007); it is stored
// rather than derived so that a future backend that cannot honour the rule
// is a data change, not a redesign.
type Share struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	// Tag is the opaque, length-limited identifier the guest addresses the
	// share by. It is derived from HostPath and must be unique within a
	// machine.
	Tag string `json:"tag"`
}

// Mode renders the share's access mode the way fstab and the CLI spell it.
func (s Share) Mode() string {
	if s.ReadOnly {
		return "ro"
	}
	return "rw"
}

// String renders a share for humans: "/Users/belli (rw)".
func (s Share) String() string { return fmt.Sprintf("%s (%s)", s.HostPath, s.Mode()) }

// NewShare builds a share for a host path, canonicalising the path and
// deriving the tag. The path must be absolute after canonicalisation; it
// need not exist yet (an unplugged disk is dropped at start, not at
// init — ADR 0007).
func NewShare(hostPath string, readOnly bool) (Share, error) {
	p, err := CanonicalHostPath(hostPath)
	if err != nil {
		return Share{}, err
	}
	return Share{HostPath: p, GuestPath: p, ReadOnly: readOnly, Tag: ShareTag(p)}, nil
}

// CanonicalHostPath turns a user-supplied path into the absolute, symlink-
// free path the guest will see. A path that does not exist is kept as its
// absolute form: it may appear later (removable media), and start decides
// what to do about it.
func CanonicalHostPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidShare)
	}
	if strings.ContainsAny(p, "\t\r\n") {
		return "", fmt.Errorf("%w: %q contains a tab or newline", ErrInvalidShare, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrInvalidShare, p, err)
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = shortFirmlinkPath(real)
	}
	if abs == "/" {
		return "", fmt.Errorf("%w: refusing to share the whole host filesystem (/)", ErrInvalidShare)
	}
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrInvalidShare, p)
	}
	return abs, nil
}

// firmlinkRoots are the macOS top-level symlinks into /private whose short
// spelling jm keeps. Only /var is in the list, and deliberately:
//
//   - /var, because the per-user temporary directory lives under it and the
//     system's own tools hand out the short name — $TMPDIR and "mktemp -d"
//     both say /var/folders/..., never /private/var/folders/.... Exporting
//     the directory under the resolved name would mean no volume argument
//     built from $TMPDIR ever matches, and "-v $(mktemp -d):/app" would
//     mount an empty guest directory (ADR 0007, identity paths).
//   - /tmp is excluded: it resolves to /private/tmp, which is what jm
//     shares, because a share mounted at the guest's own /tmp would shadow
//     it. A /tmp/... volume argument therefore has to be written
//     /private/tmp/..., which the README and "jm init --help" say.
//   - /etc is excluded: the guest's /etc is its own configuration, and jm
//     will not put host directories into it by a spelling accident.
var firmlinkRoots = []string{"/var"}

// shortFirmlinkPath maps a resolved path back to its firmlink spelling
// ("/private/var/folders/x" -> "/var/folders/x") when that is the name the
// host's own tools produce. A firmlink root itself is left resolved: the
// short name there is a directory the guest already owns.
func shortFirmlinkPath(real string) string {
	for _, root := range firmlinkRoots {
		if strings.HasPrefix(real, "/private"+root+"/") {
			return strings.TrimPrefix(real, "/private")
		}
	}
	return real
}

// ShareTag derives the stable mount tag of a host path: a readable slug so
// that guest-side diagnostics mean something, plus a hash of the full path
// so that two directories with the same basename never collide. The result
// always fits MaxTagLen.
func ShareTag(hostPath string) string {
	sum := sha256.Sum256([]byte(hostPath))
	hash := hex.EncodeToString(sum[:4]) // 8 hex characters
	slug := tagSlug(filepath.Base(hostPath))
	max := MaxTagLen - len(shareTagPrefix) - len(hash) - 2
	if len(slug) > max {
		slug = slug[:max]
	}
	return shareTagPrefix + "-" + slug + "-" + hash
}

// tagSlug keeps the characters a mount tag can safely carry.
func tagSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "share"
	}
	return out
}

// NormaliseShares canonicalises, orders and de-duplicates a share set and
// hands out unique tags.
//
// Ordering is by guest path, so a parent is always declared before the
// directories nested inside it and the guest can mount them in table
// order. A share nested inside another with the same access mode is
// redundant (the parent already puts it at its identity path) and is
// dropped; one with a different mode is kept, because it is how a user
// carves a read-only hole out of a writable tree.
func NormaliseShares(in []Share) ([]Share, error) {
	seen := map[string]bool{}
	var out []Share
	for _, s := range in {
		p, err := CanonicalHostPath(s.HostPath)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, Share{HostPath: p, GuestPath: p, ReadOnly: s.ReadOnly, Tag: ShareTag(p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostPath < out[j].HostPath })

	kept := out[:0]
	for _, s := range out {
		redundant := false
		for _, p := range kept {
			if isUnder(s.HostPath, p.HostPath) && p.ReadOnly == s.ReadOnly {
				redundant = true
				break
			}
		}
		if !redundant {
			kept = append(kept, s)
		}
	}
	if len(kept) > MaxShares {
		return nil, fmt.Errorf("%w: %d shares requested, a machine exports at most %d", ErrInvalidShare, len(kept), MaxShares)
	}
	return assignTags(kept), nil
}

// isUnder reports whether child is nested inside parent.
func isUnder(child, parent string) bool {
	return strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

// assignTags guarantees that no two shares carry the same tag. Distinct
// paths hash differently, so a clash needs a hash collision; it is handled
// anyway because a duplicate tag would silently mount the wrong directory.
func assignTags(shares []Share) []Share {
	used := map[string]bool{GuestConfTag: true}
	for i := range shares {
		tag := shares[i].Tag
		if tag == "" {
			tag = ShareTag(shares[i].HostPath)
		}
		for n := 2; used[tag]; n++ {
			suffix := fmt.Sprintf("~%d", n)
			base := shares[i].Tag
			if len(base)+len(suffix) > MaxTagLen {
				base = base[:MaxTagLen-len(suffix)]
			}
			tag = base + suffix
		}
		used[tag] = true
		shares[i].Tag = tag
	}
	return shares
}

// AddShare inserts or replaces a share and re-normalises the set.
func AddShare(list []Share, s Share) ([]Share, error) {
	out := make([]Share, 0, len(list)+1)
	for _, e := range list {
		if e.HostPath != s.HostPath {
			out = append(out, e)
		}
	}
	return NormaliseShares(append(out, s))
}

// RemoveShare drops the share for a host path, reporting whether it was
// there. The path is canonicalised first so "jm set --unmount ~/code"
// removes what "jm init --mount ~/code" added.
func RemoveShare(list []Share, hostPath string) ([]Share, bool, error) {
	p, err := CanonicalHostPath(hostPath)
	if err != nil {
		return list, false, err
	}
	out := make([]Share, 0, len(list))
	found := false
	for _, e := range list {
		if e.HostPath == p {
			found = true
			continue
		}
		out = append(out, e)
	}
	return out, found, nil
}

// SkippedShare is a share a running machine cannot export, with the reason
// to print. A vanished host path must not stop a machine booting (ADR
// 0007), so start drops it with a warning.
type SkippedShare struct {
	Share  Share
	Reason string
}

// UsableShares splits a share set into the shares a hypervisor can export
// now and the ones to skip.
func UsableShares(list []Share) (ok []Share, skipped []SkippedShare) {
	for _, s := range list {
		st, err := os.Stat(s.HostPath)
		switch {
		case err != nil:
			skipped = append(skipped, SkippedShare{Share: s, Reason: "host path is gone"})
		case !st.IsDir():
			skipped = append(skipped, SkippedShare{Share: s, Reason: "host path is not a directory"})
		default:
			ok = append(ok, s)
		}
	}
	return ok, skipped
}

// SharesTab renders the table the guest reads at boot: one tab-separated
// "<tag> <guest path> <ro|rw>" line per share, parents first. Paths cannot
// contain a tab or newline (CanonicalHostPath rejects them), so the format
// needs no quoting.
func SharesTab(list []Share) string {
	var b strings.Builder
	b.WriteString("# tag\tmountpoint\tmode — written by jm, do not edit\n")
	for _, s := range list {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", s.Tag, s.GuestPath, s.Mode())
	}
	return b.String()
}
