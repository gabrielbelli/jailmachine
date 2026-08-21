package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine"
	"github.com/gabrielbelli/jailmachine/internal/image"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// imageBuildName is the throwaway machine "jm image build" provisions. It
// lives in its own state root under --out/.work, so it cannot collide with
// the user's machines; only the podman connection name is global.
const imageBuildName = "jm-image-build"

// imageBuildSSHPort is the host port the build machine's sshd is forwarded
// to, away from the default 2222 a real machine may be using.
const imageBuildSSHPort = 2229

// sealTimeout bounds the seal script: trimming the pool's free space
// (zpool trim -w) is the slow part, minutes at most.
const sealTimeout = 15 * time.Minute

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build the prebaked guest image (maintainers)",
		Long: "Maintainer commands for the prebaked guest image that \"jm init\" fetches by\n" +
			"default. Users never need these; see docs/guest-contract.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newImageBuildCmd())
	return cmd
}

func newImageBuildCmd() *cobra.Command {
	var (
		release string
		out     string
		keep    bool
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Provision a guest from the official image, seal it and compress it",
		Long: "Build the prebaked guest image (maintainer-only; see docs/guest-contract.md):\n\n" +
			"  1. init a throwaway machine from the official FreeBSD image under --out/.work\n" +
			"  2. start it (full first-boot provisioning: packages, pf, linux, podman)\n" +
			"  3. seal it over ssh (guest/seal.sh): drop keys, host keys and logs, clean the\n" +
			"     pkg cache, restore /firstboot so the next machine's seed is applied,\n" +
			"     trim free space (zpool trim) so it compresses to nothing\n" +
			"  4. power off, compress disk.raw with zstd -19 and write the .sha256 sidecar\n\n" +
			"Output: --out/" + image.PrebakedFileName("<guest version>", "<release>") + " and its\n" +
			".sha256. Publish both as assets of the GitHub release tagged guest-<guest version>\n" +
			"(image.GuestVersion, currently " + image.GuestVersion + "). The build uses host port " +
			fmt.Sprint(imageBuildSSHPort) + " for ssh\nand registers (then removes) a podman connection named " + imageBuildName + ".",
		Example: `  jm image build
  jm image build --release 15.1-RELEASE --out dist --keep
  make image`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImageBuild(cmd.Context(), imageBuildOpts{release: release, out: out, keep: keep})
		},
	}
	f := cmd.Flags()
	f.StringVar(&release, "release", image.DefaultRelease, "FreeBSD release to build from (official image)")
	f.StringVar(&out, "out", "dist", "output directory (the build machine lives in <out>/.work)")
	f.BoolVar(&keep, "keep", false, "keep <out>/.work (the sealed disk.raw and logs) after the build")
	return cmd
}

type imageBuildOpts struct {
	release string
	out     string
	keep    bool
}

func runImageBuild(ctx context.Context, o imageBuildOpts) error {
	if err := requireBinary("zstd", "zstd"); err != nil {
		return err
	}
	out, err := filepath.Abs(o.out)
	if err != nil {
		return err
	}
	work := filepath.Join(out, ".work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	target := filepath.Join(out, image.PrebakedFileName(image.GuestVersion, o.release))
	logf(stdout, "image: building %s", target)
	logf(stdout, "image: build machine %q under %s (ssh port %d)", imageBuildName, work, imageBuildSSHPort)

	// "jm start" does not touch the default podman connection, but podman
	// itself promotes the first connection it is ever given; remember the
	// user's default and put it back afterwards in case it was ours.
	prevDefault := podmanDefaultConnection(ctx)
	defer restorePodmanDefault(ctx, prevDefault)

	// Stage: init + start, exactly as a user would, against the work root.
	sub := func(args ...string) error { return runSubcommand(ctx, work, args...) }
	if err := sub("init", "--image", "official:"+o.release, "--ssh-port", fmt.Sprint(imageBuildSSHPort), imageBuildName); err != nil {
		return fmt.Errorf("image build: init: %w", err)
	}
	if err := sub("start", imageBuildName); err != nil {
		return fmt.Errorf("image build: start: %w", err)
	}

	// Stage: seal. Runs against the work root's machine record.
	if err := withStateRoot(work, func() error { return sealGuest(ctx) }); err != nil {
		_ = sub("stop", "--force", imageBuildName)
		return err
	}

	// Stage: stop (guest poweroff, hypervisor, networking).
	if err := sub("stop", imageBuildName); err != nil {
		return fmt.Errorf("image build: stop: %w", err)
	}

	// Stage: compress + sidecar.
	disk := filepath.Join(work, machine.MachinesDir, imageBuildName, machine.DiskFile)
	if err := compressImage(ctx, disk, target); err != nil {
		return err
	}

	// Stage: tidy. rm forgets the podman connection and host key too.
	if err := sub("rm", imageBuildName); err != nil {
		fmt.Fprintf(stderr, "jm: warning: removing the build machine: %v\n", err)
	}
	if !o.keep {
		if err := os.RemoveAll(work); err != nil {
			return err
		}
	} else {
		logf(stdout, "image: kept %s", work)
	}
	logf(stdout, "done: publish %s and %s.sha256 as assets of release guest-%s", target, target, image.GuestVersion)
	return nil
}

// runSubcommand executes "jm --state-root <root> <args...>" in-process, so
// image build reuses init/start/stop/rm unchanged (including their locks,
// stage lines and error messages).
func runSubcommand(ctx context.Context, root string, args ...string) error {
	saved := stateRoot
	defer func() { stateRoot = saved }()
	full := []string{"--state-root", root}
	if quiet {
		full = append(full, "--quiet")
	}
	cmd := NewRootCmd()
	cmd.SetArgs(append(full, args...))
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd.ExecuteContext(ctx)
}

// withStateRoot runs fn with the global state root pointed at root.
func withStateRoot(root string, fn func() error) error {
	saved := stateRoot
	stateRoot = root
	defer func() { stateRoot = saved }()
	return fn()
}

// sealGuest runs guest/seal.sh over ssh on the build machine and checks
// the guest is still in the state the contract requires (ready marker
// present, failure marker absent).
func sealGuest(ctx context.Context) error {
	m, err := store().Load(imageBuildName)
	if err != nil {
		return err
	}
	ep, err := endpointOf(m)
	if err != nil {
		return err
	}
	logf(stdout, "seal: running guest/seal.sh over ssh (trims free space with zpool trim; a few minutes)")
	sctx, cancel := context.WithTimeout(ctx, sealTimeout)
	defer cancel()
	c, err := sshx.Dial(sctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	defer c.Close()
	// The script drops root's authorized_keys, so this is the last session
	// this key can open; everything happens in one command.
	_, stderrOut, err := c.Run(sctx, "sh -c "+shellQuote(jailmachine.SealScript)+" 2>&1 | tail -n 40")
	if err != nil {
		return fmt.Errorf("seal: %w\n%s", err, stderrOut)
	}
	// The seal script runs unattended; verify the contract afterwards in
	// the same connection (a fresh Dial would be refused now).
	checks := "test -f " + machine.GuestProvisionMarker +
		" && test ! -e " + machine.GuestProvisionFailed +
		" && test -e /firstboot && test ! -e /root/.ssh/authorized_keys"
	if _, _, err := c.Run(sctx, checks); err != nil {
		return errors.New("seal: guest is not in the sealed state (marker, /firstboot, authorized_keys); see guest/seal.sh")
	}
	logf(stdout, "seal: done")
	return nil
}

// compressImage writes disk compressed with zstd to target plus a
// target+".sha256" sidecar, and prints the sizes.
func compressImage(ctx context.Context, disk, target string) error {
	st, err := os.Stat(disk)
	if err != nil {
		return fmt.Errorf("image build: %w", err)
	}
	logf(stdout, "compress: zstd -T0 -19 %s (%s logical) -> %s", disk, humanBytes(st.Size()), target)
	tmp := target + ".part"
	cmd := exec.CommandContext(ctx, "zstd", "-T0", "-19", "--sparse", "-q", "-f", "-o", tmp, disk)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("image build: zstd: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	sum, err := image.SHA256File(target)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target+".sha256", []byte(image.SidecarLine(sum, target)), 0o644); err != nil {
		return err
	}
	out, err := os.Stat(target)
	if err != nil {
		return err
	}
	logf(stdout, "compress: %s (%s), sha256 %s", target, humanBytes(out.Size()), sum)
	return nil
}

// podmanDefaultConnection returns the name of the host podman's default
// connection, or "" when there is none (or podman is missing).
func podmanDefaultConnection(ctx context.Context) string {
	out, err := podman(ctx, "system", "connection", "list", "--format", "json")
	if err != nil {
		return ""
	}
	var conns []struct {
		Name    string `json:"Name"`
		Default bool   `json:"Default"`
	}
	if json.Unmarshal([]byte(out), &conns) != nil {
		return ""
	}
	for _, c := range conns {
		if c.Default && c.Name != imageBuildName {
			return c.Name
		}
	}
	return ""
}

// restorePodmanDefault makes name the default podman connection again.
func restorePodmanDefault(ctx context.Context, name string) {
	if name == "" {
		return
	}
	if _, err := podman(ctx, "system", "connection", "default", name); err != nil {
		fmt.Fprintf(stderr, "jm: warning: could not restore podman default connection %q: %v\n", name, err)
	}
}

// shellQuote wraps s in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// humanBytes renders n in binary units with one decimal.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
