package cli

import (
	"context"
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
			"an interruption: finished steps are skipped.",
		Example: `  jm init
  jm init --cpus 2 --memory 2048 dev
  jm init --image official:14.3-RELEASE --disk 32`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd.Context(), args, initOpts{
				image: imageRef, cpus: cpus, memory: memory, disk: disk, sshPort: sshPort,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&imageRef, "image", machine.DefaultImage, "image source, e.g. official or official:"+image.DefaultRelease)
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
// reference with the release resolved, so the record says exactly which
// image was fetched ("official:15.1-RELEASE", never a floating "official").
func imageSource(ref machine.ImageRef, diskGiB int) (image.Source, machine.ImageRef, error) {
	switch ref.Source {
	case "official":
		if ref.Release == "" {
			ref.Release = image.DefaultRelease
		}
		return &image.Official{Release: ref.Release, DiskGiB: diskGiB}, ref, nil
	default:
		return nil, ref, usagef("unknown image source %q (known: official)", ref.Source)
	}
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
	ref, err := machine.ParseImageRef(o.image)
	if err != nil {
		return usage(err)
	}
	src, ref, err := imageSource(ref, o.disk)
	if err != nil {
		return err
	}
	// The backend is chosen per host OS (override: $JM_BACKEND) and recorded
	// in the machine; it checks its own host prerequisites (ADR 0002).
	backendName := backend.DefaultForHost()
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

	s := store()
	if s.Exists(name) {
		return withHint(fmt.Errorf("machine %q already exists", name), fmt.Sprintf("run 'jm rm%s' first, or pick another name", nameHint(name)))
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
	if _, err := os.Stat(diskPath); err != nil {
		logf(stdout, "image: fetching %s", m.Image)
		if err := src.Fetch(ctx, diskPath, progressOut()); err != nil {
			return withHint(fmt.Errorf("image: %w", err), fmt.Sprintf("re-run 'jm init%s'; a partial download resumes where it stopped", nameHint(name)))
		}
	} else {
		logf(stdout, "image: reusing existing %s", diskPath)
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
