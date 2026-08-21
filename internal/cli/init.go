package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine"
	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/image"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/seed"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

func newInitCmd() *cobra.Command {
	d := machine.Defaults()
	var (
		imageRef string
		cpus     int
		memory   int
		disk     int
		sshPort  int
	)
	cmd := &cobra.Command{
		Use:   "init [name]",
		Short: "Create a new machine (download image, write seed)",
		Long: "Create a machine: generate an SSH key, download and verify the FreeBSD image,\n" +
			"grow it to --disk, and write the first-boot NoCloud seed. Safe to re-run after\n" +
			"an interruption: finished steps are skipped.\n\n" +
			"Image sources: \"prebaked\" (default) is a provisioned guest published on the\n" +
			"project's GitHub releases (first boot in seconds); \"official\" is the stock\n" +
			"FreeBSD BASIC-CLOUDINIT image, provisioned on first boot (minutes); a path or\n" +
			"https URL to a .raw, .raw.xz or .raw.zst is used as is, verified against a\n" +
			"sibling .sha256 when one exists and marked untrusted otherwise.",
		Example: `  jm init
  jm init --cpus 2 --memory 2048 dev
  jm init --image official:` + image.DefaultRelease + ` --disk 32
  jm init --image prebaked:` + image.GuestVersion + `
  jm init --image ~/Downloads/custom.raw.zst   # verified if custom.raw.zst.sha256 sits next to it`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd.Context(), args, initOpts{
				image: imageRef, cpus: cpus, memory: memory, disk: disk, sshPort: sshPort,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&imageRef, "image", machine.DefaultImage, "image source: prebaked[:<guest version>], official[:<release>], or a path/URL to a .raw[.xz|.zst]")
	f.IntVar(&cpus, "cpus", d.CPUs, "number of virtual CPUs")
	f.IntVar(&memory, "memory", d.MemoryMiB, "memory in MiB")
	f.IntVar(&disk, "disk", d.DiskGiB, "disk size in GiB")
	f.IntVar(&sshPort, "ssh-port", d.SSHPort, "host port forwarded to the guest's sshd")
	cmd.Long += "\nThe network provider is chosen per host ($JM_NETWORK overrides; known: " +
		strings.Join(netprov.Names(), ", ") + ")."
	return cmd
}

type initOpts struct {
	image   string
	cpus    int
	memory  int
	disk    int
	sshPort int
}

func (o initOpts) validate() error {
	switch {
	case o.cpus < 1:
		return usagef("--cpus must be at least 1")
	case o.memory < 256:
		return usagef("--memory must be at least 256 MiB")
	case o.disk < 1:
		return usagef("--disk must be at least 1 GiB")
	case o.sshPort < 1 || o.sshPort > 65535:
		return usagef("--ssh-port must be between 1 and 65535")
	}
	return nil
}

// imageSource maps a parsed --image reference to a provider and returns the
// reference with the version resolved, so the record says exactly which
// image was fetched ("prebaked:15.1.0", "official:15.1-RELEASE", never a
// floating name). A path or URL is a bring-your-own image recorded as is.
func imageSource(ref machine.ImageRef, diskGiB int) (image.Source, machine.ImageRef, error) {
	switch ref.Source {
	case "prebaked":
		if ref.Release == "" {
			ref.Release = image.GuestVersion
		}
		return &image.Prebaked{Version: ref.Release, DiskGiB: diskGiB}, ref, nil
	case "official":
		if ref.Release == "" {
			ref.Release = image.DefaultRelease
		}
		return &image.Official{Release: ref.Release, DiskGiB: diskGiB}, ref, nil
	case "byo":
		return &image.BYO{Ref: ref.Release, DiskGiB: diskGiB}, ref, nil
	default:
		return nil, ref, usagef("unknown image source %q (known: prebaked, official, or a path/URL to a .raw, .raw.xz or .raw.zst)", ref.Source)
	}
}

// parseImage turns the --image flag into a reference: named sources go
// through machine.ParseImageRef; paths and URLs become a "byo" reference
// whose Release field carries the path or URL verbatim.
func parseImage(flag string) (machine.ImageRef, error) {
	if image.IsBYORef(flag) {
		return machine.ImageRef{Source: "byo", Release: flag}, nil
	}
	ref, err := machine.ParseImageRef(flag)
	if err != nil {
		return ref, usage(err)
	}
	return ref, nil
}

// fetchHint picks the hint for a failed image fetch. A prebaked image whose
// release is not published (no .sha256 sidecar at the release URL) cannot be
// fixed by re-running, so point at the official image instead.
func fetchHint(name string, ref machine.ImageRef, err error) string {
	if ref.Source == "prebaked" && errors.Is(err, image.ErrNoChecksum) {
		return fmt.Sprintf("guest release guest-%s is not published (or has no .sha256 sidecar); try 'jm init%s --image official' to provision the stock FreeBSD image on first boot", ref.Release, nameHint(name))
	}
	return fmt.Sprintf("re-run 'jm init%s'; a partial download resumes where it stopped", nameHint(name))
}

func runInit(ctx context.Context, args []string, o initOpts) error {
	// init never falls back to "the only machine": no name means create
	// the default one.
	name, err := machine.ResolveName(args)
	if err != nil {
		return usage(err)
	}
	activeMachine = name
	if err := o.validate(); err != nil {
		return err
	}
	ref, err := parseImage(o.image)
	if err != nil {
		return err
	}
	src, ref, err := imageSource(ref, o.disk)
	if err != nil {
		return err
	}
	// Refuse an existing name before touching the host: no backend or
	// tool check is needed to say "already exists".
	s := store()
	if s.Exists(name) {
		return withHint(fmt.Errorf("machine %q already exists", name), fmt.Sprintf("run 'jm rm%s' first, or pick another name", nameHint(name)))
	}
	// The backend is chosen per host OS (override: $JM_BACKEND) and recorded
	// in the machine; it checks its own host prerequisites (ADR 0002).
	backendName, err := backend.DefaultForHost()
	if err != nil {
		return err
	}
	b, err := backend.Get(backendName)
	if err != nil {
		return err
	}
	if err := b.Preflight(); err != nil {
		return err
	}
	// Likewise the network provider (override: $JM_NETWORK), fixed at init
	// so a machine keeps the networking it was created with (ADR 0004).
	networkName := netprov.DefaultForHost()
	p, err := netprov.Get(networkName)
	if err != nil {
		return err
	}
	if err := p.Preflight(); err != nil {
		return err
	}
	if err := requireBinary("podman", "podman"); err != nil {
		return err
	}

	unlock, err := lock(name)
	if err != nil {
		return err
	}
	defer unlock()

	m := machine.Defaults()
	m.Name = name
	m.Backend = backendName
	m.Network = networkName
	m.Image = ref.String()
	m.CPUs = o.cpus
	m.MemoryMiB = o.memory
	m.DiskGiB = o.disk
	m.SSHPort = o.sshPort
	m.Created = time.Now().UTC()

	// Stage: SSH identity (kept across re-runs so a seed already written
	// stays valid).
	key := s.Path(name, machine.SSHKeyFile)
	if _, err := os.Stat(key); err != nil {
		logf(stdout, "ssh-key: generating %s", key)
		if err := sshx.GenerateKey(key); err != nil {
			return err
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}

	// Stage: disk image. Fetch is atomic, so an existing disk.raw is done.
	diskPath := s.Path(name, machine.DiskFile)
	untrustedPath := s.Path(name, machine.ImageUntrustedFile)
	if _, err := os.Stat(diskPath); err != nil {
		logf(stdout, "image: fetching %s", m.Image)
		if err := src.Fetch(ctx, diskPath, progressOut()); err != nil {
			return withHint(fmt.Errorf("image: %w", err), fetchHint(name, ref, err))
		}
		// Trust is a property of the source (ADR 0003): the checksummed
		// sources fail rather than install an unverified image; BYO
		// installs it and says so. The verdict is persisted next to the
		// disk so a re-run after an interruption (before the record is
		// saved) cannot promote the reused disk to trusted.
		if t, ok := src.(image.Trust); ok && !t.Trusted() {
			m.ImageTrusted = false
			if err := os.WriteFile(untrustedPath, []byte(ref.Release+"\n"), 0o600); err != nil {
				return fmt.Errorf("image: %w", err)
			}
			fmt.Fprintf(stderr, "jm: warning: %s was not verified against a checksum (no .sha256 sidecar); inspect shows image_trusted=false\n", ref.Release)
		}
	} else {
		logf(stdout, "image: reusing existing %s", diskPath)
		if _, err := os.Stat(untrustedPath); err == nil {
			m.ImageTrusted = false
			fmt.Fprintf(stderr, "jm: warning: %s was installed without a checksum; inspect shows image_trusted=false\n", diskPath)
		}
	}

	// Stage: first-boot seed.
	logf(stdout, "seed: writing first-boot seed %s", s.Path(name, machine.SeedFile))
	err = seed.Build(s.Path(name, machine.SeedFile), seed.Params{
		InstanceID:      name,
		Hostname:        name,
		SSHPubKey:       strings.TrimSpace(string(pub)),
		ProvisionScript: jailmachine.ProvisionScript,
	})
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	if err := s.Save(&m); err != nil {
		return err
	}
	logf(stdout, "done: created %s (%d cpus, %d MiB, %d GiB). Next: jm start%s", name, m.CPUs, m.MemoryMiB, m.DiskGiB, nameHint(name))
	return nil
}
