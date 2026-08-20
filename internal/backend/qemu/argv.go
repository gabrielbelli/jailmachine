package qemu

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

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

// MaxSocketPath is the longest unix socket path we allow: sun_path is 104
// bytes including the NUL on macOS and the BSDs (108 on Linux).
const MaxSocketPath = 103

// QMPSocket returns the QMP socket path for a machine directory. It lives in
// the directory when that fits in sun_path; otherwise (deep --state-root,
// long machine name) it falls back to a short, deterministic path under the
// system temp dir, so State/Stop/Repair always agree on where to look.
func QMPSocket(dir string) string {
	if p := filepath.Join(dir, QMPSockFile); len(p) <= MaxSocketPath {
		return p
	}
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(os.TempDir(), "jm-"+hex.EncodeToString(sum[:6])+".sock")
}

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
}

// Accel returns the hardware accelerator for the current host OS.
func Accel() string {
	switch runtime.GOOS {
	case "darwin":
		return "hvf"
	case "linux":
		return "kvm"
	default:
		return "tcg"
	}
}

// Args builds the qemu-system-aarch64 argument vector, mirroring bin/jm
// cmd_start exactly.
func Args(m *machine.Machine, net backend.NetAttachment, p Paths) []string {
	mac := net.MAC
	if mac == "" {
		mac = m.MAC
	}
	args := []string{
		"-M", "virt,accel=" + Accel(),
		"-cpu", "host",
		"-smp", fmt.Sprint(m.CPUs),
		"-m", fmt.Sprint(m.MemoryMiB),
		"-drive", "if=pflash,format=raw,readonly=on,file=" + p.Code,
		"-drive", "if=pflash,format=raw,file=" + p.Vars,
		"-drive", "file=" + p.Disk + ",format=raw,if=virtio,cache=writeback,discard=unmap",
		"-drive", "file=" + p.Seed + ",format=raw,if=virtio,readonly=on",
	}
	args = append(args, netdevArgs(net, mac)...)
	args = append(args,
		"-device", "virtio-rng-pci",
		"-display", "none",
		"-serial", "file:"+p.Console,
		"-qmp", "unix:"+p.QMP+",server,nowait",
		"-daemonize",
		"-pidfile", p.PID,
	)
	return args
}

func netdevArgs(net backend.NetAttachment, mac string) []string {
	switch net.Kind {
	case "", "user":
		return []string{
			"-netdev", fmt.Sprintf("user,id=n0,hostfwd=tcp:127.0.0.1:%d-:22", net.HostFwdSSH),
			"-device", "virtio-net-pci,netdev=n0,mac=" + mac,
		}
	default:
		// Unknown kinds (gvproxy, vmnet) arrive with later providers; boot
		// without a NIC rather than guess, so the failure is visible.
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
