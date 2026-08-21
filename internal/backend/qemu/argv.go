package qemu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// Binary is the QEMU system emulator we drive.
const Binary = "qemu-system-aarch64"

// EDK2 firmware files shipped with QEMU (share/qemu).
const (
	FirmwareCode = "edk2-aarch64-code.fd"
	FirmwareVars = "edk2-arm-vars.fd"
)

// FallbackFirmwareDir is tried when the share dir cannot be derived from the
// binary location (matches the PoC's /opt/homebrew/share/qemu).
const FallbackFirmwareDir = "/opt/homebrew/share/qemu"

// Files this backend keeps in the machine directory, besides the
// backend-neutral ones in package machine.
const (
	LogFile     = "qemu.log" // qemu's stdout/stderr
	PIDFile     = "qemu.pid" // written by -pidfile
	QMPSockFile = "qmp.sock" // QMP unix socket (see QMPSocket)
)

// MaxSocketPath is backend.MaxSocketPath, kept for callers of this package.
const MaxSocketPath = backend.MaxSocketPath

// QMPSocket returns the QMP socket path for a machine directory, using the
// shared sun_path fallback rule (backend.SocketPath) so State/Stop/Repair
// always agree on where to look.
func QMPSocket(dir string) string { return backend.SocketPath(dir, QMPSockFile) }

// Paths are the host files an invocation needs. Args is a pure function of
// a Machine, a NetAttachment and Paths so it can be unit-tested.
type Paths struct {
	Code    string // read-only EDK2 code pflash
	Vars    string // per-machine EDK2 variable store
	Disk    string // disk.raw
	Seed    string // seed.iso
	Console string // console.log
	QMP     string // qmp.sock
	PID     string // qemu.pid
	Log     string // qemu.log (not passed to qemu; captured from its stderr)
	// GuestConf is the host directory exported read-only to the guest
	// under machine.GuestConfTag; it carries the share table (ADR 0007).
	// Empty means the machine shares nothing.
	GuestConf string
}

// AccelEnv overrides the accelerator, e.g. JM_QEMU_ACCEL=tcg to run the
// guest under pure emulation where no hypervisor is available (CI runners
// without HVF/KVM, the guest-image build job). TCG is an order of magnitude
// slower than HVF; it is for building images, not for using them.
const AccelEnv = "JM_QEMU_ACCEL"

// AccelTCG is QEMU's software emulator.
const AccelTCG = "tcg"

// Accel returns the accelerator for the current host OS, or the $JM_QEMU_ACCEL
// override.
func Accel() string {
	if v := os.Getenv(AccelEnv); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "darwin":
		return "hvf"
	case "linux":
		return "kvm"
	default:
		return AccelTCG
	}
}

// CPUModel is the -cpu value that goes with an accelerator: "host" passes
// the real CPU through under HVF/KVM; TCG has no host CPU to pass through
// and needs a named model that FreeBSD/arm64 boots on.
func CPUModel(accel string) string {
	if accel == AccelTCG {
		return "cortex-a72"
	}
	return "host"
}

// Args builds the qemu-system-aarch64 argument vector, mirroring bin/jm
// cmd_start exactly.
func Args(m *machine.Machine, net backend.NetAttachment, p Paths) []string {
	mac := net.MAC
	if mac == "" {
		mac = m.MAC
	}
	accel := Accel()
	args := []string{
		"-M", "virt,accel=" + accel,
		"-cpu", CPUModel(accel),
		"-smp", fmt.Sprint(m.CPUs),
		"-m", fmt.Sprint(m.MemoryMiB),
		"-drive", "if=pflash,format=raw,readonly=on,file=" + escapeComma(p.Code),
		"-drive", "if=pflash,format=raw,file=" + escapeComma(p.Vars),
		"-drive", "file=" + escapeComma(p.Disk) + ",format=raw,if=virtio,cache=writeback,discard=unmap",
		"-drive", "file=" + escapeComma(p.Seed) + ",format=raw,if=virtio,readonly=on",
	}
	args = append(args, netdevArgs(net, mac)...)
	args = append(args, "-device", "virtio-rng-pci")
	args = append(args, shareArgs(m, p)...)
	args = append(args,
		"-display", "none",
		"-serial", "file:"+escapeComma(p.Console),
		"-qmp", "unix:"+escapeComma(p.QMP)+",server,nowait",
		"-daemonize",
		"-pidfile", p.PID,
	)
	return args
}

// Host filesystem sharing (ADR 0007): one -fsdev/-device pair per share.
//
//   - security_model= chooses how guest metadata is stored: none applies the
//     guest's modes to the host file, the mapped models keep them in xattrs
//     (or a sidecar). See ShareSecurityModel below for why mapped-xattr wins.
//   - multidevs=remap keeps inode numbers unique when one export spans
//     several host filesystems (a home directory with a mounted volume
//     under it), which the 9p protocol otherwise cannot express.
//   - addr= is pinned, and this is not cosmetic. QEMU hands the slots on
//     the PCIe root bus to explicit -device arguments before the drives
//     created by -drive if=virtio, so an unpinned 9p device moves the root
//     disk to a different slot, the EFI boot entry recorded in efivars.fd
//     no longer resolves and the firmware deletes it: the machine never
//     boots again. Pinning the share devices above every automatic slot
//     leaves the disks exactly where the firmware last saw them.
const (
	// ShareAddrBase is the first PCI slot reserved for share devices.
	ShareAddrBase = 0x8
	// ShareFsdevPrefix names the -fsdev backends.
	ShareFsdevPrefix = "jmfs"
	// ConfFsdevID is the -fsdev backend carrying the share table.
	ConfFsdevID = "jmconf"
	// ShareSecurityModel is the 9p security model for the share devices.
	//
	// "none" passes the host's modes straight through, which reads nicely
	// on the Mac but breaks any container that relies on being root: the
	// host end of the share runs as the unprivileged Mac user, so a file
	// the container has just made read-only cannot be written again, and
	// macOS enforces that even for the file's owner. Git does exactly this
	// with its pack temp files, so "git clone" into a shared directory
	// fails outright.
	//
	// "mapped-xattr" keeps the guest's ownership and modes in xattrs
	// instead, so root in a container behaves as it does on Linux. The
	// cost is cosmetic and host-side: a file a container creates shows up
	// on the Mac as 0600 with user.virtfs.* xattrs, its real mode being
	// the one the guest sees. Files the Mac creates keep their own modes
	// and ownership in the guest, and a non-root container reads them.
	// $JM_9P_SECURITY takes "none" or "mapped-file" for the other trade.
	ShareSecurityModel = "mapped-xattr"
)

// shareArgs builds the 9p devices for a machine's shares. The first device
// is always the read-only configuration share: it carries the share table
// that tells the guest which mount tag belongs at which path, so the guest
// mounts everything itself at boot rather than waiting for jm to push a
// script over SSH. A machine with no shares gets no 9p hardware at all.
func shareArgs(m *machine.Machine, p Paths) []string {
	if len(m.Shares) == 0 || p.GuestConf == "" {
		return nil
	}
	var args []string
	addr := ShareAddrBase
	add := func(id, hostPath, tag string, readOnly bool) {
		fsdev := "local,id=" + id + ",path=" + escapeComma(hostPath) +
			",security_model=" + shareSecurityModel() + ",multidevs=remap"
		if readOnly {
			fsdev += ",readonly=on"
		}
		args = append(args,
			"-fsdev", fsdev,
			"-device", fmt.Sprintf("virtio-9p-pci,fsdev=%s,mount_tag=%s,addr=0x%x", id, tag, addr),
		)
		addr++
	}
	add(ConfFsdevID, p.GuestConf, machine.GuestConfTag, true)
	for i, s := range m.Shares {
		add(fmt.Sprintf("%s%d", ShareFsdevPrefix, i), s.HostPath, s.Tag, s.ReadOnly)
	}
	return args
}

// escapeComma doubles commas in a path so QEMU's option parser does not
// split on them (QEMU's convention: ",," is a literal comma).
func escapeComma(path string) string { return strings.ReplaceAll(path, ",", ",,") }

func netdevArgs(net backend.NetAttachment, mac string) []string {
	switch net.Kind {
	case "", backend.KindUser:
		addr := net.HostFwdAddr
		if addr == "" {
			addr = backend.DefaultHostFwdAddr
		}
		return []string{
			"-netdev", fmt.Sprintf("user,id=n0,hostfwd=tcp:%s:%d-:22", addr, net.HostFwdSSH),
			"-device", "virtio-net-pci,netdev=n0,mac=" + mac,
		}
	case backend.KindStream:
		// gvproxy (or any userspace stack) listens on a unix socket; QEMU
		// connects to it, so the provider must be up before Start.
		return []string{
			"-netdev", "stream,id=n0,addr.type=unix,addr.path=" + escapeComma(net.SocketPath),
			"-device", "virtio-net-pci,netdev=n0,mac=" + mac,
		}
	default:
		// Unknown kinds (vmnet) arrive with later providers; boot without a
		// NIC rather than guess, so the failure is visible.
		return []string{"-nic", "none"}
	}
}

// LookupBinary finds qemu-system-aarch64 on PATH.
func LookupBinary() (string, error) {
	bin, err := exec.LookPath(Binary)
	if err != nil {
		return "", fmt.Errorf("qemu: %s not found on PATH (brew install qemu): %w", Binary, err)
	}
	return bin, nil
}

// FirmwareDir locates QEMU's share/qemu directory relative to the binary
// (<prefix>/bin/qemu-system-aarch64 -> <prefix>/share/qemu), following
// symlinks if needed, and falls back to FallbackFirmwareDir.
func FirmwareDir(bin string) (string, error) {
	candidates := []string{shareFor(bin)}
	if real, err := filepath.EvalSymlinks(bin); err == nil && real != bin {
		candidates = append(candidates, shareFor(real))
	}
	candidates = append(candidates, FallbackFirmwareDir)
	for _, d := range candidates {
		if _, err := os.Stat(filepath.Join(d, FirmwareCode)); err == nil {
			return d, nil
		}
	}
	return "", errors.New("qemu: cannot find " + FirmwareCode + " (looked in " + fmt.Sprint(candidates) + ")")
}

func shareFor(bin string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(bin)), "share", "qemu")
}

// shareSecurityModel returns the 9p security model for the share devices,
// overridable with $JM_9P_SECURITY ("none", "mapped-xattr", "mapped-file").
func shareSecurityModel() string {
	switch v := strings.TrimSpace(os.Getenv("JM_9P_SECURITY")); v {
	case "none", "mapped-xattr", "mapped-file":
		return v
	default:
		return ShareSecurityModel
	}
}
