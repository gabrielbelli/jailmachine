package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/doctor"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/resolver"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Name resolution parity (ADR 0008). "jm start" runs a host-side resolver
// per machine, detached like the port forwarder, and points the guest at it:
// every name the host resolves, the guest and its containers resolve the
// same way, because the answer comes from the host's own resolver.

// stageDNS is the start stage that wires the guest's name resolution up. It
// is a stage name, not a new lifecycle concept: the constant lives here so
// the shared lifecycle file does not have to change.
const stageDNS = machine.Stage("dns")

// searchPoll is how often the running resolver re-reads the host's search
// list. Joining or leaving a VPN changes it, and the guest has to follow
// without a restart (ADR 0008).
const searchPoll = 30 * time.Second

// guestDNSTimeout bounds the guest-side configuration step.
const guestDNSTimeout = 2 * time.Minute

// newResolverCmd is the hidden foreground entry point of the host resolver.
// "jm start" launches it detached; it runs until SIGTERM/SIGINT (jm stop)
// and logs to stdout, which the launcher points at resolver.log.
func newResolverCmd() *cobra.Command {
	return &cobra.Command{
		Use:    resolver.Command + " [name]",
		Short:  "Answer the guest's DNS queries from the host resolver (internal)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			ep, err := endpointOf(m)
			if err != nil {
				return err
			}
			return runResolver(cmd.Context(), m, ep)
		},
	}
}

func runResolver(ctx context.Context, m *machine.Machine, ep netprov.Endpoint) error {
	logger := log.New(stdout, "resolver: ", log.LstdFlags)
	hostAlias, err := netip.ParseAddr(ep.HostAlias)
	if err != nil {
		return fmt.Errorf("resolver: network %q offers no host alias to answer on: %w", m.NetworkName(), err)
	}
	gateway, _ := netip.ParseAddr(ep.Gateway)
	names := resolver.HostNames(ctx)
	h := resolver.NewHandler(resolver.Config{
		HostAlias: hostAlias,
		Gateway:   gateway,
		Aliases:   resolver.DefaultAliases(hostAlias, gateway, names),
		// The provider's network is IPv4 only: an AAAA answer would be an
		// address the guest cannot route to (ADR 0008).
		AllowIPv6: false,
		Log:       logger,
	})
	pr := resolverProcess(m)
	// Reusing last run's port keeps the guest's configuration valid across
	// a restart, so a rebooted guest resolves before jm start reaches it.
	srv, err := resolver.Listen(h, "127.0.0.1", pr.LastPort())
	if err != nil {
		return err
	}
	defer srv.Close()
	if err := pr.PublishAddr(srv.Addr()); err != nil {
		return err
	}
	if !resolver.HostResolver {
		logger.Printf("WARNING: built with the netgo build tag; queries go through Go's own DNS client, " +
			"which does not see scoped resolvers, /etc/hosts or .local names")
	}
	logger.Printf("listening on %s for %s (host alias %s, host names %s, pid %d)",
		srv.Addr(), m.Name, hostAlias, strings.Join(names, " "), os.Getpid())
	defer logger.Printf("stopped")
	go watchHost(ctx, m, ep, h, srv.Port(), logger)
	return srv.Serve(ctx)
}

// watchHost re-reads the host's resolver configuration and follows it: the
// search list is pushed into the guest and the host's own names are kept in
// the alias table, so joining or leaving a VPN converges without a restart
// (ADR 0008). Containers already running keep the list they were created
// with, a documented limitation.
func watchHost(ctx context.Context, m *machine.Machine, ep netprov.Endpoint, h *resolver.Handler, port int, logger *log.Logger) {
	search := resolver.SearchDomains(ctx)
	names := resolver.HostNames(ctx)
	hostAlias, _ := netip.ParseAddr(ep.HostAlias)
	gateway, _ := netip.ParseAddr(ep.Gateway)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(searchPoll):
		}
		if now := resolver.HostNames(ctx); !reflect.DeepEqual(now, names) {
			names = now
			h.SetAliases(resolver.DefaultAliases(hostAlias, gateway, names))
			logger.Printf("host names changed: %s", strings.Join(names, " "))
		}
		now := resolver.SearchDomains(ctx)
		if reflect.DeepEqual(now, search) {
			continue
		}
		search = now
		logger.Printf("host search list changed: %s", strings.Join(search, " "))
		cctx, cancel := context.WithTimeout(ctx, guestDNSTimeout)
		client, err := sshx.Dial(cctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
		if err == nil {
			err = applyGuestDNS(cctx, client, ep, port, search)
			_ = client.Close()
		}
		cancel()
		if err != nil {
			logger.Printf("pushing the search list into the guest: %v", err)
		}
	}
}

// resolverProcess locates m's detached resolver.
func resolverProcess(m *machine.Machine) resolver.Process {
	return resolver.Process{Dir: m.Dir, Name: m.Name, Root: StateRoot()}
}

// startResolver is the host half of the dns stage: launch the detached
// resolver and return the port it bound. Providers with no route back to
// the host (slirp) cannot carry guest DNS to it; the stage says so and the
// guest keeps the provider's own resolver.
func startResolver(ctx context.Context, m *machine.Machine, ep netprov.Endpoint) (int, error) {
	if ep.HostAlias == "" {
		return 0, nil
	}
	// jmBinary, not os.Executable: a helper launched from the "jpodman" or
	// "jdocker" symlink must run as jm.
	exe, err := jmBinary()
	if err != nil {
		return 0, machine.NewStageError(stageDNS, "", fmt.Errorf("locating the jm binary: %w", err))
	}
	pr := resolverProcess(m)
	if err := pr.Start(ctx, exe); err != nil {
		return 0, machine.NewStageError(stageDNS, "see "+pr.LogPath(), err)
	}
	return pr.Port(), nil
}

// stopResolver terminates the resolver; a stopped or absent one is tidied
// away. It is called before the hypervisor and the provider are stopped.
func stopResolver(ctx context.Context, m *machine.Machine) {
	if m.Dir == "" {
		return
	}
	if err := resolverProcess(m).Stop(ctx); err != nil {
		fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
	}
}

// configureGuestDNS is the guest half of the dns stage: point the guest and
// its containers at the host resolver, then prove that a name only the host
// can answer really resolves in the guest.
func configureGuestDNS(ctx context.Context, m *machine.Machine, client *sshx.Client, ep netprov.Endpoint, port int) error {
	if port == 0 {
		logf(stdout, "%s: %s networking cannot route guest DNS to the host; skipping", stageDNS, m.NetworkName())
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, guestDNSTimeout)
	defer cancel()
	search := resolver.SearchDomains(ctx)
	logf(stdout, "%s: pointing the guest at the host resolver on %s:%d%s",
		stageDNS, ep.HostAlias, port, searchSuffix(search))
	err := applyGuestDNS(ctx, client, ep, port, search)
	if err == nil {
		err = verifyGuestDNS(ctx, client, ep)
	}
	if err == nil {
		return nil
	}
	// Name resolution parity is a feature, not a precondition for booting:
	// a guest that satisfies the ADR 0003 contract but has no working
	// local_unbound (a bring-your-own image), or one whose forwarder failed
	// transiently, must still start. The guest-side script only takes over
	// /etc/resolv.conf once the forwarder answers, so such a guest keeps
	// the resolution it already had; say so and let "jm doctor" report the
	// loss (ADR 0008).
	//
	// The exception is a guest whose /etc/resolv.conf jm already owns:
	// there is nothing to fall back to, so a dead resolver there is a
	// broken machine and start must fail.
	if owned, oerr := jmOwnsResolvConf(ctx, client); oerr == nil && owned {
		return machine.NewStageError(stageDNS, resolverHint(m), err)
	}
	fmt.Fprintf(stderr, "jm: warning: %s: %v; %s keeps the name resolution it booted with, "+
		"so VPN, /etc/hosts and .local names may not resolve in it or its containers "+
		"(%s); 'jm doctor' re-checks parity\n", stageDNS, err, m.Name, resolverHint(m))
	return nil
}

// jmOwnsResolvConf reports whether the guest's /etc/resolv.conf is one jm
// wrote (see resolver.OwnsResolvConfCmd).
func jmOwnsResolvConf(ctx context.Context, client *sshx.Client) (bool, error) {
	return client.Succeeds(ctx, resolver.OwnsResolvConfCmd())
}

func searchSuffix(search []string) string {
	if len(search) == 0 {
		return ""
	}
	return " (search " + strings.Join(search, " ") + ")"
}

func resolverHint(m *machine.Machine) string {
	return fmt.Sprintf("see %s, and 'jm ssh%s service local_unbound status'",
		resolverProcess(m).LogPath(), nameHint(m.Name))
}

// applyGuestDNS runs the guest-side configuration script. It is idempotent:
// nothing is rewritten and nothing is restarted when the configuration has
// not changed.
func applyGuestDNS(ctx context.Context, client *sshx.Client, ep netprov.Endpoint, port int, search []string) error {
	g := resolver.GuestConfig{
		UpstreamIP:   ep.HostAlias,
		UpstreamPort: port,
		Nameserver:   ep.GuestIP,
		HostAlias:    ep.HostAlias,
		Search:       search,
	}
	script, err := g.Script()
	if err != nil {
		return err
	}
	out, errOut, err := client.RunScript(ctx, "the guest name-resolution script", script)
	if err != nil {
		if detail := firstNonEmpty(errOut, out); detail != "" {
			return fmt.Errorf("%w: %s", err, lastLine(detail))
		}
		return err
	}
	return nil
}

// verifyGuestDNS asserts parity rather than liveness: it compares what the
// guest resolves the host alias to with what it must be. A resolver that
// merely answers is not parity (ADR 0008).
func verifyGuestDNS(ctx context.Context, client *sshx.Client, ep netprov.Endpoint) error {
	cmd := resolver.VerifyCommand(resolver.ProbeName, ep.GuestIP)
	out, errOut, err := client.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("resolving %s in the guest: %w: %s", resolver.ProbeName, err, firstNonEmpty(errOut, out))
	}
	got := strings.Fields(out)
	for _, a := range got {
		if a == ep.HostAlias {
			return nil
		}
	}
	return fmt.Errorf("the guest resolves %s to %q, not to the host at %s",
		resolver.ProbeName, strings.Join(got, " "), ep.HostAlias)
}

// lastLine keeps the end of a captured output, where a shell script's real
// complaint is, so a warning stays one line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// resolverState reports whether m's host resolver is running, for inspect
// and doctor.
func resolverState(m *machine.Machine) backend.State {
	if m.Dir == "" {
		return backend.Stopped
	}
	if _, ok := resolverProcess(m).Alive(); ok {
		return backend.Running
	}
	return backend.Stopped
}

// errNoResolver is what doctor reports when parity cannot even be asked
// about.
var errNoResolver = errors.New("no host resolver is running for this machine")

// resolverParityCheck asserts, for a running machine, that the host
// resolver really answers from the host's own resolution API: it resolves a
// name only the host can know — the Mac's own multicast name — through the
// machine's resolver and compares the address, not merely that an answer
// came back. A resolver that merely runs, or one that quietly fell back to
// a public server, would pass a liveness check and fail this one (ADR 0008).
func resolverParityCheck(ctx context.Context, m *machine.Machine) (doctor.Result, bool) {
	res := doctor.Result{Name: "resolver " + m.Name}
	if resolverState(m) != backend.Running {
		return res, false
	}
	pr := resolverProcess(m)
	addr := pr.Addr()
	if addr == "" {
		res.Status, res.Detail = doctor.Warn, errNoResolver.Error()
		res.Fix = "jm start" + nameHint(m.Name)
		return res, true
	}
	ep, err := endpointOf(m)
	if err != nil {
		res.Status, res.Detail = doctor.Warn, err.Error()
		return res, true
	}
	names := resolver.HostNames(ctx)
	probe := resolver.ProbeName
	for _, n := range names {
		if strings.HasSuffix(n, ".local") {
			probe = n
			break
		}
	}
	ips, err := queryResolver(ctx, addr, probe)
	if err != nil {
		res.Status, res.Detail = doctor.Fail, fmt.Sprintf("%s does not answer %s: %v", addr, probe, err)
		res.Fix = "see " + pr.LogPath() + "; 'jm stop" + nameHint(m.Name) + " && jm start" + nameHint(m.Name) + "' restarts it"
		return res, true
	}
	for _, ip := range ips {
		if ip == ep.HostAlias {
			res.Status = doctor.OK
			res.Detail = fmt.Sprintf("%s answers %s with %s, the host", addr, probe, ip)
			return res, true
		}
	}
	res.Status = doctor.Fail
	res.Detail = fmt.Sprintf("%s answers %s with %v, not with the host at %s", addr, probe, ips, ep.HostAlias)
	res.Fix = "see " + pr.LogPath() + "; a jm built with '-tags netgo' cannot see the host's own names"
	return res, true
}

// guestResolverParityCheck asserts that the guest itself — and therefore
// every container in it — really resolves through the host resolver, rather
// than through whatever it booted with. It is the other half of
// resolverParityCheck, which only proves that the host-side resolver
// answers: "jm start" degrades to a warning when the guest-side forwarder
// will not come up (a bring-your-own image, a transient failure), so this is
// what reports the loss afterwards (ADR 0008).
func guestResolverParityCheck(ctx context.Context, m *machine.Machine) (doctor.Result, bool) {
	res := doctor.Result{Name: "guest resolver " + m.Name}
	if st, err := currentState(m); err != nil || st != backend.Running {
		return res, false
	}
	ep, err := endpointOf(m)
	if err != nil || ep.HostAlias == "" {
		// A provider with no route back to the host never configures the
		// guest at all; the dns stage says so at start.
		return res, false
	}
	ctx, cancel := context.WithTimeout(ctx, guestProbeTimeout)
	defer cancel()
	client, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		res.Status, res.Detail = doctor.Warn, fmt.Sprintf("cannot reach the guest to check: %v", err)
		res.Fix = "jm start" + nameHint(m.Name)
		return res, true
	}
	defer client.Close()
	if err := verifyGuestDNS(ctx, client, ep); err != nil {
		res.Status, res.Detail = doctor.Fail, err.Error()
		res.Fix = resolverHint(m) + "; 'jm stop" + nameHint(m.Name) + " && jm start" + nameHint(m.Name) + "' reconfigures it"
		return res, true
	}
	res.Status = doctor.OK
	res.Detail = fmt.Sprintf("the guest resolves %s to %s, the host", resolver.ProbeName, ep.HostAlias)
	return res, true
}

// guestProbeTimeout bounds one guest-side doctor probe.
const guestProbeTimeout = 30 * time.Second

// queryResolver asks one machine's host resolver for a name's IPv4
// addresses, going nowhere near the host's normal resolution path.
func queryResolver(ctx context.Context, addr, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	ips, err := r.LookupIP(ctx, "ip4", strings.TrimSuffix(name, ".")+".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out, nil
}
