package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/image"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Resource limits accepted by set (and shared with init's ranges).
const (
	minCPUs      = 1
	maxCPUs      = 256
	minMemoryMiB = 256
	maxMemoryMiB = 1 << 20 // 1 TiB
	minDiskGiB   = 1
	maxDiskGiB   = 1 << 14 // 16 TiB
	growTimeout  = 2 * time.Minute
)

func newSetCmd() *cobra.Command {
	var o setOpts
	cmd := &cobra.Command{
		Use:   "set [name]",
		Short: "Change a machine's resources",
		Long: "Change CPUs, memory, disk size, the SSH port or the shared host\n" +
			"directories of a machine.\n" +
			"--cpus, --memory, --ssh-port, --mount and --unmount need the machine stopped.\n" +
			"--disk only grows (disk.raw is extended sparsely); on a running machine the\n" +
			"guest's partition and ZFS pool are extended at once, otherwise on the next\n" +
			"'jm start'.\n\n" +
			"A shared directory appears in the guest at its own absolute path, so\n" +
			"'-v /work/src:/app' resolves inside the guest unchanged. The share set takes\n" +
			"effect on the next start; jm says so.\n\n" +
			"--publish-addr sets the host address container ports are published on (the\n" +
			"default is every interface, as docker does on Linux); it applies when the\n" +
			"forwarder is next started.",
		Example: `  jm set --cpus 8 --memory 8GiB
  jm set --mount /work --mount /srv/data:ro
  jm set --unmount /srv/data
  jm set --publish-addr 127.0.0.1   # keep published ports off the LAN`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.cpusSet = cmd.Flags().Changed("cpus")
			o.memorySet = cmd.Flags().Changed("memory")
			o.diskSet = cmd.Flags().Changed("disk")
			o.sshPortSet = cmd.Flags().Changed("ssh-port")
			o.publishAddrSet = cmd.Flags().Changed("publish-addr")
			return runSet(cmd.Context(), args, o)
		},
	}
	f := cmd.Flags()
	f.IntVar(&o.cpus, "cpus", 0, "number of virtual CPUs")
	f.StringVar(&o.memory, "memory", "", "memory: MiB, or with a unit (4096MiB, 4GiB, 4g)")
	f.IntVar(&o.disk, "disk", 0, "disk size in GiB (grow only)")
	f.IntVar(&o.sshPort, "ssh-port", 0, "host port forwarded to the guest's sshd")
	f.StringArrayVar(&o.mount, "mount", nil, mountFlagUsage)
	f.StringArrayVar(&o.unmount, "unmount", nil, "stop sharing a host directory (repeatable)")
	f.StringVar(&o.publishAddr, "publish-addr", "", publishAddrFlagUsage)
	return cmd
}

type setOpts struct {
	cpus, disk, sshPort                     int
	memory                                  string
	publishAddr                             string
	mount, unmount                          []string
	cpusSet, memorySet, diskSet, sshPortSet bool
	publishAddrSet                          bool
}

// ParseMemoryMiB parses a memory size: a bare number is MiB; suffixes
// m/mib/mb and g/gib/gb (any case, optional space) scale it.
func ParseMemoryMiB(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, errors.New("empty memory size")
	}
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	n, err := strconv.Atoi(num)
	if err != nil || num == "" || n < 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	mult := 1
	switch unit {
	case "", "m", "mib", "mb":
	case "g", "gib", "gb":
		mult = 1024
	default:
		return 0, fmt.Errorf("invalid memory unit %q in %q (use MiB or GiB)", unit, s)
	}
	if n > maxMemoryMiB {
		return 0, fmt.Errorf("memory size %q is too large", s)
	}
	return n * mult, nil
}

// changes is the validated result of a set invocation applied to m.
type changes struct {
	cpus, memoryMiB, diskGiB, sshPort       int
	cpusSet, memorySet, diskSet, sshPortSet bool
	// publishAddr is the host address published ports bind to; it takes
	// effect when the forwarder is next started, so it needs no stop.
	publishAddr    string
	publishAddrSet bool
	// shares is the new share set; sharesSet says whether --mount or
	// --unmount was given at all (an empty set is a legitimate result).
	shares    []machine.Share
	sharesSet bool
}

// any reports whether at least one flag was given.
func (c changes) any() bool {
	return c.cpusSet || c.memorySet || c.diskSet || c.sshPortSet || c.sharesSet || c.publishAddrSet
}

// needsStopped reports whether the changes require a stopped machine. The
// share set is part of the virtual hardware, so it changes only between
// boots.
func (c changes) needsStopped() bool {
	return c.cpusSet || c.memorySet || c.sshPortSet || c.sharesSet
}

// validate parses and range-checks the flags against the current record.
func (o setOpts) validate(m *machine.Machine) (changes, error) {
	c := changes{
		cpusSet: o.cpusSet, memorySet: o.memorySet, diskSet: o.diskSet, sshPortSet: o.sshPortSet,
		publishAddrSet: o.publishAddrSet,
		sharesSet:      len(o.mount) > 0 || len(o.unmount) > 0,
	}
	if !c.any() {
		return c, errors.New("nothing to set (use --cpus, --memory, --disk, --ssh-port, --publish-addr, --mount or --unmount)")
	}
	if o.publishAddrSet {
		addr, err := parsePublishAddr(o.publishAddr)
		if err != nil {
			return c, err
		}
		c.publishAddr = addr
	}
	if c.sharesSet {
		shares, err := applyMounts(m.Shares, o.mount, o.unmount)
		if err != nil {
			return c, err
		}
		c.shares = shares
	}
	if o.cpusSet {
		if o.cpus < minCPUs || o.cpus > maxCPUs {
			return c, fmt.Errorf("--cpus must be between %d and %d", minCPUs, maxCPUs)
		}
		c.cpus = o.cpus
	}
	if o.memorySet {
		mib, err := ParseMemoryMiB(o.memory)
		if err != nil {
			return c, fmt.Errorf("--memory: %w", err)
		}
		if mib < minMemoryMiB || mib > maxMemoryMiB {
			return c, fmt.Errorf("--memory must be between %d MiB and %d MiB", minMemoryMiB, maxMemoryMiB)
		}
		c.memoryMiB = mib
	}
	if o.diskSet {
		switch {
		case o.disk < minDiskGiB || o.disk > maxDiskGiB:
			return c, fmt.Errorf("--disk must be between %d and %d GiB", minDiskGiB, maxDiskGiB)
		case o.disk < m.DiskGiB:
			return c, fmt.Errorf("--disk can only grow: %s has %d GiB, %d GiB requested", m.Name, m.DiskGiB, o.disk)
		}
		c.diskGiB = o.disk
	}
	if o.sshPortSet {
		if o.sshPort < 1 || o.sshPort > 65535 {
			return c, errors.New("--ssh-port must be between 1 and 65535")
		}
		c.sshPort = o.sshPort
	}
	return c, nil
}

func runSet(ctx context.Context, args []string, o setOpts) error {
	m, err := loadMachine(args)
	if err != nil {
		return err
	}
	c, err := o.validate(m)
	if err != nil {
		return usage(err)
	}
	unlock, err := lock(m.Name)
	if err != nil {
		return err
	}
	defer unlock()

	st, err := currentState(m)
	if err != nil {
		return err
	}
	stopHint := "stop the machine first: jm stop" + nameHint(m.Name)
	if st != backend.Stopped && c.needsStopped() {
		return withHint(fmt.Errorf("%s is %s; cpus, memory, the ssh port and the shared directories change only on a stopped machine", m.Name, st), stopHint)
	}

	if c.cpusSet && c.cpus != m.CPUs {
		logf(stdout, "cpus: %d -> %d", m.CPUs, c.cpus)
		m.CPUs = c.cpus
	}
	if c.memorySet && c.memoryMiB != m.MemoryMiB {
		logf(stdout, "memory: %d MiB -> %d MiB", m.MemoryMiB, c.memoryMiB)
		m.MemoryMiB = c.memoryMiB
	}
	if c.sshPortSet && c.sshPort != m.SSHPort {
		logf(stdout, "ssh port: %d -> %d", m.SSHPort, c.sshPort)
		forgetHostKey(m)
		m.SSHPort = c.sshPort
	}
	if c.sharesSet && !sameShares(c.shares, m.Shares) {
		b, err := backendFor(m)
		if err != nil {
			return err
		}
		m.Shares = c.shares
		warnUnsupportedShares(m, b)
		warnMissingShares(m.Shares)
		if len(m.Shares) == 0 {
			logf(stdout, "shares: none")
		}
		for _, sh := range m.Shares {
			logf(stdout, "share: %s", sh)
		}
		logf(stdout, "the shared directories are attached on the next start: jm start%s", nameHint(m.Name))
	}
	if c.publishAddrSet && c.publishAddr != m.PublishAddr {
		m.PublishAddr = c.publishAddr
		logf(stdout, "publish address: %s", forwarder.HostIP(m.PublishAddr))
		if st == backend.Running {
			logf(stdout, "%s", publishAddrNote(m))
		}
	}
	if c.diskSet && c.diskGiB != m.DiskGiB {
		var resizer backend.Resizer
		switch st {
		case backend.Stopped:
		case backend.Running:
			b, err := backendFor(m)
			if err != nil {
				return err
			}
			var ok bool
			if resizer, ok = b.(backend.Resizer); !ok {
				return withHint(fmt.Errorf("%s is %s and backend %q cannot grow a live disk", m.Name, st, b.Name()), stopHint)
			}
		default:
			// Broken (stale pid, half-up components): a live grow could
			// not reach the hypervisor or the guest; stop repairs it.
			return withHint(fmt.Errorf("%s is %s; the disk grows only on a stopped or running machine", m.Name, st), stopHint)
		}
		logf(stdout, "disk: %d GiB -> %d GiB", m.DiskGiB, c.diskGiB)
		if err := image.Grow(store().Path(m.Name, machine.DiskFile), int64(c.diskGiB)<<30); err != nil {
			return err
		}
		m.DiskGiB = c.diskGiB
		// The record is saved before the guest side so a failed or
		// interrupted grow is retried by start, never forgotten.
		m.SetPendingGrow(true)
		if err := store().Save(m); err != nil {
			return err
		}
		if st == backend.Running {
			// The hypervisor reads the image size at boot; tell it about
			// the new size before the guest looks.
			if err := resizer.ResizeDisk(ctx, m, int64(c.diskGiB)<<30); err != nil {
				return fmt.Errorf("disk.raw grown but the hypervisor did not pick it up (retried on the next start): %w", err)
			}
			if err := growGuest(ctx, m); err != nil {
				return fmt.Errorf("disk.raw grown but the guest did not pick it up (retried on the next start): %w", err)
			}
			m.SetPendingGrow(false)
		} else {
			logf(stdout, "the guest's partition and pool are extended on the next start")
		}
	}
	if err := store().Save(m); err != nil {
		return err
	}
	logf(stdout, "%s: %d cpus, %d MiB, %d GiB, ssh port %d, publishing on %s",
		m.Name, m.CPUs, m.MemoryMiB, m.DiskGiB, m.SSHPort, forwarder.HostIP(m.PublishAddr))
	return nil
}

// growGuest runs the partition/pool grow in a running guest over SSH.
func growGuest(ctx context.Context, m *machine.Machine) error {
	ctx, cancel := context.WithTimeout(ctx, growTimeout)
	defer cancel()
	ep, err := endpointOf(m)
	if err != nil {
		return err
	}
	c, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		return err
	}
	defer c.Close()
	return growGuestWith(ctx, m, c)
}

// growGuestWith is growGuest on an existing connection (used by start).
func growGuestWith(ctx context.Context, m *machine.Machine, c *sshx.Client) error {
	logf(stdout, "extending %s's partition and zroot to %d GiB", m.Name, m.DiskGiB)
	out, errOut, err := c.Run(ctx, machine.GuestGrowCmd(int64(m.DiskGiB)<<30))
	if err != nil {
		msg := strings.TrimSpace(errOut + "\n" + out)
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// finishPendingGrow is the start-side half of "jm set --disk" on a stopped
// machine: once sshd answers, the guest is told about the bigger disk and
// the flag is cleared. Failure does not stop start; the flag stays so the
// next start retries.
func finishPendingGrow(ctx context.Context, m *machine.Machine, c *sshx.Client) {
	if !m.PendingGrow() {
		return
	}
	// Warnings go to stderr regardless of --quiet: the start succeeds but
	// the disk is not what the record says.
	if err := growGuestWith(ctx, m, c); err != nil {
		fmt.Fprintf(stderr, "jm: warning: guest disk grow failed, will retry on the next start: %v\n", err)
		return
	}
	m.SetPendingGrow(false)
	if err := store().Save(m); err != nil {
		fmt.Fprintf(stderr, "jm: warning: %v\n", err)
	}
}
