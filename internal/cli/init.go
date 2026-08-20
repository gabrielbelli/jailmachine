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
		return fmt.Errorf("--cpus must be at least 1")
	case o.memory < 256:
		return fmt.Errorf("--memory must be at least 256 MiB")
	case o.disk < 1:
		return fmt.Errorf("--disk must be at least 1 GiB")
	case o.sshPort < 1 || o.sshPort > 65535:
		return fmt.Errorf("--ssh-port must be between 1 and 65535")
	}
	return nil
}

// imageSource maps a parsed --image reference to a provider.
func imageSource(ref machine.ImageRef, diskGiB int) (image.Source, error) {
	switch ref.Source {
	case "official":
		return &image.Official{Release: ref.Release, DiskGiB: diskGiB}, nil
	default:
		return nil, fmt.Errorf("unknown image source %q (known: official)", ref.Source)
	}
}

func runInit(ctx context.Context, args []string, o initOpts) error {
	name, err := machine.ResolveName(args)
	if err != nil {
		return err
	}
	if err := o.validate(); err != nil {
		return err
	}
	ref, err := machine.ParseImageRef(o.image)
	if err != nil {
		return err
	}
	src, err := imageSource(ref, o.disk)
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
	if err := requireBinary("podman", "podman"); err != nil {
		return err
	}

	s := store()
	if s.Exists(name) {
		return fmt.Errorf("machine %q already exists (run 'jm rm%s' first)", name, nameHint(name))
	}
	unlock, err := lock(name)
	if err != nil {
		return err
	}
	defer unlock()

	m := machine.Defaults()
	m.Name = name
	m.Backend = backendName
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
		logf(stdout, "generating SSH key")
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
		logf(stdout, "fetching image %s", m.Image)
		if err := src.Fetch(ctx, diskPath, stdout); err != nil {
			return fmt.Errorf("fetch image: %w", err)
		}
	} else {
		logf(stdout, "reusing existing %s", diskPath)
	}

	// Stage: first-boot seed.
	logf(stdout, "writing cloud-init seed")
	err = seed.Build(s.Path(name, machine.SeedFile), seed.Params{
		InstanceID:      name,
		Hostname:        name,
		SSHPubKey:       strings.TrimSpace(string(pub)),
		ProvisionScript: jailmachine.ProvisionScript,
	})
	if err != nil {
		return err
	}

	if err := s.Save(&m); err != nil {
		return err
	}
	logf(stdout, "created %s. Next: jm start%s", name, nameHint(name))
	return nil
}
