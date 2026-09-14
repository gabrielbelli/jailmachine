package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// A saved state can only be loaded into the device model it was taken from,
// so a resume reuses the exact argv of the launch it was saved from, with the
// machine type pinned to a versioned model ("virt-11.1" rather than the
// "virt" alias, which moves with every QEMU release).

// AbsentShareDir is where resolveSavedArgv points a share whose host path is
// gone, under the machine directory: <dir>/guest/absent/<tag>.
const AbsentShareDir = "absent"

// machineFlagIndex returns the index of the value of -M or -machine.
func machineFlagIndex(argv []string) int {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-M" || argv[i] == "-machine" {
			return i + 1
		}
	}
	return -1
}

// MachineTypeOf returns the value of -M up to the first comma ("virt" or
// "virt-11.1").
func MachineTypeOf(argv []string) (string, error) {
	i := machineFlagIndex(argv)
	if i < 0 {
		return "", errors.New("qemu: argv has no -M")
	}
	t := splitOpts(argv[i])[0]
	t = strings.TrimPrefix(t, "type=")
	if t == "" {
		return "", fmt.Errorf("qemu: argv has an empty machine type (-M %s)", argv[i])
	}
	return t, nil
}

// versionedMachineType reports whether t names a versioned model
// ("virt-11.1") rather than an alias ("virt").
func versionedMachineType(t string) bool {
	i := strings.LastIndexByte(t, '-')
	return i > 0 && i+1 < len(t) && t[i+1] >= '0' && t[i+1] <= '9'
}

// aliasResolver is the part of Monitor PinMachineType needs.
type aliasResolver interface {
	AliasTarget(ctx context.Context, alias string) (string, error)
}

// PinMachineType returns the versioned machine type of argv: kept when argv
// already names one (a machine resumed before keeps its model across a QEMU
// upgrade), otherwise the alias resolved by the running QEMU itself.
func PinMachineType(ctx context.Context, argv []string, mon aliasResolver) (string, error) {
	t, err := MachineTypeOf(argv)
	if err != nil {
		return "", err
	}
	if versionedMachineType(t) {
		return t, nil
	}
	target, err := mon.AliasTarget(ctx, t)
	if err != nil {
		return "", fmt.Errorf("qemu: resolving machine type %q: %w", t, err)
	}
	if !versionedMachineType(target) {
		return "", fmt.Errorf("qemu: machine type %q resolves to %q, which has no version", t, target)
	}
	return target, nil
}

// withMachineType returns a copy of argv whose -M type is machineType, the
// rest of the value (",accel=hvf") kept.
func withMachineType(argv []string, machineType string) ([]string, error) {
	out := slices.Clone(argv)
	i := machineFlagIndex(out)
	if i < 0 {
		return nil, errors.New("qemu: argv has no -M")
	}
	opts := splitOpts(out[i])
	opts[0] = machineType
	out[i] = strings.Join(opts, ",")
	return out, nil
}

// hasIncoming reports whether argv starts QEMU waiting for a migration.
func hasIncoming(argv []string) bool { return slices.Contains(argv, "-incoming") }

// IncomingArgv copies saved, replaces -M's type with machineType keeping the
// rest (",accel=hvf"), refuses -daemonize or an -incoming other than a
// previous resume's "-incoming defer" (which it strips), and appends
// "-incoming", "defer".
func IncomingArgv(saved []string, machineType string) ([]string, error) {
	if slices.Contains(saved, "-daemonize") {
		return nil, errors.New("qemu: saved argv daemonises QEMU")
	}
	stripped := make([]string, 0, len(saved)+2)
	for i := 0; i < len(saved); i++ {
		if saved[i] == "-incoming" {
			if i+1 < len(saved) && saved[i+1] == "defer" {
				i++
				continue
			}
			return nil, fmt.Errorf("qemu: saved argv already has -incoming %q", strings.Join(saved[i+1:min(i+2, len(saved))], ""))
		}
		stripped = append(stripped, saved[i])
	}
	out, err := withMachineType(stripped, machineType)
	if err != nil {
		return nil, err
	}
	return append(out, "-incoming", "defer"), nil
}

// resolveSavedArgv adapts a saved argv to host changes made while the machine
// was suspended, without changing the device model: an -fsdev whose path no
// longer exists is pointed at an empty read-only placeholder
// <dir>/guest/absent/<tag> (created here), and a firmware code image that has
// moved (a QEMU upgrade) is looked for in fwDir. Device ids and addr= are
// untouched. It returns the new argv and the mount tags of the placeholders.
func resolveSavedArgv(argv []string, dir, fwDir string) ([]string, []string, error) {
	out := slices.Clone(argv)
	// fsdev id -> mount tag, from the -device lines.
	tags := map[string]string{}
	for i := 0; i+1 < len(out); i++ {
		if out[i] != "-device" {
			continue
		}
		opts := splitOpts(out[i+1])
		if !strings.HasPrefix(opts[0], "virtio-9p") {
			continue
		}
		if id, tag := optGet(opts, "fsdev"), optGet(opts, "mount_tag"); id != "" && tag != "" {
			tags[id] = tag
		}
	}
	var absent []string
	for i := 0; i+1 < len(out); i++ {
		switch out[i] {
		case "-fsdev":
			opts := splitOpts(out[i+1])
			path := optGet(opts, "path")
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err == nil {
				continue
			}
			tag := tags[optGet(opts, "id")]
			if tag == "" {
				tag = optGet(opts, "id")
			}
			if tag == "" || strings.ContainsAny(tag, `/\`) {
				return nil, nil, fmt.Errorf("qemu: saved share %s has no usable tag", path)
			}
			placeholder := filepath.Join(dir, machine.GuestConfDir, AbsentShareDir, tag)
			if err := os.MkdirAll(placeholder, 0o755); err != nil {
				return nil, nil, fmt.Errorf("qemu: creating share placeholder: %w", err)
			}
			opts = optSet(opts, "path", placeholder)
			if optGet(opts, "readonly") != "on" {
				opts = optSet(opts, "readonly", "on")
			}
			out[i+1] = strings.Join(opts, ",")
			absent = append(absent, tag)
		case "-drive":
			opts := splitOpts(out[i+1])
			file := optGet(opts, "file")
			if optGet(opts, "if") != "pflash" || optGet(opts, "readonly") != "on" || file == "" || fwDir == "" {
				continue
			}
			if _, err := os.Stat(file); err == nil {
				continue
			}
			moved := filepath.Join(fwDir, filepath.Base(file))
			if _, err := os.Stat(moved); err != nil {
				moved = filepath.Join(fwDir, FirmwareCode)
			}
			out[i+1] = strings.Join(optSet(opts, "file", moved), ",")
		}
	}
	return out, absent, nil
}

// splitOpts splits a QEMU option string on single commas, keeping each
// element escaped (",," stays ",,"). The result has at least one element.
func splitOpts(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ',' {
			continue
		}
		if i+1 < len(s) && s[i+1] == ',' {
			i++
			continue
		}
		out = append(out, s[start:i])
		start = i + 1
	}
	return append(out, s[start:])
}

// optGet returns the unescaped value of key in split options, "" if absent.
func optGet(opts []string, key string) string {
	for _, o := range opts {
		if v, ok := strings.CutPrefix(o, key+"="); ok {
			return strings.ReplaceAll(v, ",,", ",")
		}
	}
	return ""
}

// optSet sets key to value (escaped) in place, or appends it.
func optSet(opts []string, key, value string) []string {
	out := slices.Clone(opts)
	for i, o := range out {
		if strings.HasPrefix(o, key+"=") {
			out[i] = key + "=" + escapeComma(value)
			return out
		}
	}
	return append(out, key+"="+escapeComma(value))
}
