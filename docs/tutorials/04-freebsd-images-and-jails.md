<!-- linked from docs/tutorials/README.md -->
# 4. Native FreeBSD images, and jails

Build a container image with **no Linux in it at all**, push it to a registry,
then build the other kind of FreeBSD workload — a **bastille jail** — and work
out which one you actually want.

| | |
|---|---|
| You need | A machine from [tutorial 1](01-first-container.md), started |
| Time | About 20 minutes |
| You end with | A FreeBSD image in a registry, a jail you created and destroyed, and a rule for choosing between them |

---

## Part 1 — build a native FreeBSD image

Native FreeBSD images need no `--os` flag (see
[tutorial 1](01-first-container.md#why---oslinux-and-why-freebsd-images-need-nothing))
and hit none of the Linuxulator's gaps. The FreeBSD project publishes the
bases:

| Base | Use it for |
|---|---|
| `ghcr.io/freebsd/freebsd-runtime:15.1` | The normal choice — has a working `pkg` |
| `ghcr.io/freebsd/freebsd-runtime:14.3` | Same, on the 14 branch |
| `docker.io/dougrabson/freebsd15-minimal` | Static binaries only; **no `pkg` runtime** |

```bash
mkdir -p ~/jm-freebsd && cd ~/jm-freebsd

cat > Containerfile <<'EOF'
FROM ghcr.io/freebsd/freebsd-runtime:15.1
RUN env ASSUME_ALWAYS_YES=yes pkg bootstrap -f \
 && pkg install -y curl \
 && pkg clean -ay
COPY hello.sh /usr/local/bin/hello
CMD ["/usr/local/bin/hello"]
EOF

cat > hello.sh <<'EOF'
#!/bin/sh
echo "uname:  $(uname -srm)"
echo "jailed: $(sysctl -n security.jail.jailed)"
echo "curl:   $(pkg query '%n-%v' curl)"
echo "https:  $(curl -sS -o /dev/null -w '%{http_code}' https://www.freebsd.org/)"
EOF
chmod +x hello.sh

jpodman build -t freebsd-hello:1.0 .
```

```text
STEP 1/4: FROM ghcr.io/freebsd/freebsd-runtime:15.1
STEP 2/4: RUN env ASSUME_ALWAYS_YES=yes pkg bootstrap -f  && pkg install -y curl  && pkg clean -ay
--> dd3891538cda
STEP 3/4: COPY hello.sh /usr/local/bin/hello
--> d4c9f4d10371
STEP 4/4: CMD ["/usr/local/bin/hello"]
Successfully tagged localhost/freebsd-hello:1.0
```

The first build took **16.5 s**, almost all of it `pkg install`; a rebuild
with the `RUN` layer cached took 1.4 s.

```bash
jpodman run --rm freebsd-hello:1.0
```

```text
uname:  FreeBSD 15.1-RELEASE-p2 arm64
jailed: 1
curl:   curl-8.21.0
https:  200
```

Three things in four lines:

- `uname` is the **guest's own kernel**. There is no second kernel and no
  translation layer — this is a FreeBSD process on a FreeBSD kernel.
- `jailed: 1`. On FreeBSD a container **is a jail**: `ocijail` creates one, and
  the jail is the isolation boundary. That is the same primitive part 2 uses.
- `https: 200` — outbound networking and DNS work from the container, through
  gvproxy and the host resolver.

> `RUN --mount=type=cache` is **not** supported here — buildah on FreeBSD
> answers `cache mounts not supported on freebsd`. `RUN --mount=type=secret`
> does work.

---

## Part 2 — push it to a registry

### The offline loop: a registry in the machine

Run a registry as a container and push into it. Two jailmachine-specific
details make this less obvious than it looks:

**Pick a host port that is not 5000.** On macOS, Control Centre's AirPlay
Receiver listens there, and `jm ports` says so exactly:

```text
0.0.0.0:5000  192.168.127.2:5000  tcp    error: another process on this Mac already
holds this host port (lsof -nP -iTCP:5000); publish the container on a different host port
```

**Push to the registry's container IP, not to `localhost`.** The push is made
by the engine **inside the guest**, and the guest cannot reach a published
container port over its own loopback:

```bash
jpodman run -d --os=linux --name registry -p 5001:5000 docker.io/library/registry:2

jm ssh -- 'fetch -qo- --timeout=5 http://127.0.0.1:5001/v2/_catalog'   # from the guest
curl -s http://localhost:5001/v2/_catalog                              # from the Mac
```

```text
fetch: transfer timed out
{"repositories":[]}
```

The Mac reaches it (through jm's forwarder); the guest does not — and the
guest's own `192.168.127.2:5001` times out too. This is podman-on-FreeBSD's
CNI portmap plugin, not jm; the full measurement is in
[TROUBLESHOOTING](../TROUBLESHOOTING.md#a-published-port-is-not-reachable-from-the-guest).
Two addresses do work from inside the guest: `host.containers.internal:5001`,
which goes back out through the Mac's forwarder, and — the one used here —
the container's own address on the guest's bridge:

```bash
REG=$(jpodman inspect registry --format '{{.NetworkSettings.IPAddress}}')
echo "$REG"

jpodman tag  freebsd-hello:1.0 "$REG:5000/freebsd-hello:1.0"
jpodman push --tls-verify=false "$REG:5000/freebsd-hello:1.0"
```

```text
10.88.0.3
Copying blob sha256:78ec2c2213da8caacb51d9cf235ccc60a70bb9dbe7876e0c9f3e8b336ab8bfca
Copying config sha256:e2514d39b6e6aa5590daf7d7c9f3b274219f4b4a77b9f48786416d78b1ee9e49
Writing manifest to image destination
```

Confirm from the Mac, then pull it back and run it:

```bash
curl -s http://localhost:5001/v2/_catalog
curl -s http://localhost:5001/v2/freebsd-hello/tags/list

jpodman rmi "$REG:5000/freebsd-hello:1.0"
jpodman pull --tls-verify=false "$REG:5000/freebsd-hello:1.0"
jpodman run  --rm "$REG:5000/freebsd-hello:1.0"
```

```text
{"repositories":["freebsd-hello"]}
{"name":"freebsd-hello","tags":["1.0"]}
uname:  FreeBSD 15.1-RELEASE-p2 arm64
jailed: 1
curl:   curl-8.21.0
https:  200
```

### The real thing: GHCR

Pushing to a public registry is the same three commands, made against the
guest's engine like everything else:

```bash
gh auth token | jpodman login ghcr.io -u <your-github-user> --password-stdin
jpodman tag  freebsd-hello:1.0 ghcr.io/<your-github-user>/freebsd-hello:1.0
jpodman push ghcr.io/<your-github-user>/freebsd-hello:1.0
```

> **These three were not run while writing this page** — they would publish a
> package under a real account. Everything above them was.

There is one trap worth knowing before you do it, because it has no fix after
the fact:

**A GHCR package first created by a personal token is private *and*
unconnected to any repository — and an unconnected package cannot be made
public.** You end up with an image nobody can pull and a settings page that
will not let you fix it.

The way round it is to let CI create the package. A push made from GitHub
Actions with the workflow's `GITHUB_TOKEN` connects the package to the
repository and gives it the repository's visibility; a later manual push only
*adds tags* to a package that is already public and already linked. That is
exactly what this repository does — `.github/workflows/demo-images.yml`
pushes a placeholder `:bootstrap` tag from Actions for the images that can
only be built on a Mac, precisely so the manual push lands on an existing
public package. See [demo/README.md](../../demo/README.md#how-these-images-are-published).

If you have already created a private unconnected package, delete it and let
CI create it again.

---

## Part 3 — the other kind of FreeBSD workload: a jail

`bastille` is installed and configured in the guest by provisioning: ZFS
datasets under `zroot/bastille`, a `bastille0` loopback, and NAT through `pf`.
Nothing below needs setting up.

```bash
jm ssh -- 'ifconfig bastille0 | head -3; grep -A1 "table <jails>" /etc/pf.conf'
```

```text
bastille0: flags=1008049<UP,LOOPBACK,RUNNING,MULTICAST,LOWER_UP> metric 0 mtu 16384
	options=680003<RXCSUM,TXCSUM,LINKSTATE,RXCSUM_IPV6,TXCSUM_IPV6>
	inet 10.17.89.10 netmask 0xffffffff
table <jails> persist
nat on $v4egress_if inet from <jails> to any -> ($v4egress_if)
```

Jails are managed **from inside the guest**; host-side jail management
(`jm jail ...`) is deliberately out of MVP scope
([ADR 0006](../adr/0006-scope-boundaries.md)), so everything goes through
`jm ssh`.

### Bootstrap a release

```bash
jm ssh -- bastille bootstrap 15.1-RELEASE
```

```text
Bootstrapping FreeBSD release: 15.1-RELEASE

Bootstrap appears complete!
```

The first bootstrap downloads `base.txz` for the release; it lands as 360 MiB
in `zroot/bastille/releases` and takes minutes rather than seconds (the run
above was already cached, and finished in 1.2 s). Every jail you create
afterwards is a **ZFS clone** of it.

### Create, use, destroy

```bash
jm ssh -- bastille create demo 15.1-RELEASE 10.17.89.10
jm ssh -- bastille list all
```

```text
Template applied: default/thin

[demo]:
demo: created

 JID  Boot  Prio  State  IP Address   Published Ports  Hostname  Release       Path
 17   on    99    Up     10.17.89.10  -                demo      15.1-RELEASE  /usr/local/bastille/jails/demo/root
```

Creation took **3.8 s** — it is a ZFS clone, not a copy. (`bastille list -a`
is deprecated in bastille 1.4.4; use `bastille list all`.)

```bash
jm ssh -- 'bastille cmd demo uname -srm; bastille cmd demo hostname'
jm ssh -- bastille cmd demo pkg install -y curl
jm ssh -- 'bastille cmd demo curl -sS -o /dev/null -w "%{http_code}\n" https://www.freebsd.org/'
```

```text
[demo]:
FreeBSD 15.1-RELEASE arm64

[demo]:
demo

[demo] [10/10] Installing curl-8.21.0...
[demo] [10/10] Extracting curl-8.21.0: .......... done

[demo]:
200
```

`pkg install` took 12 s and reached the network through the `pf` NAT rule
above. The important part is what happens next: **the package is still there.**
A jail is a long-lived FreeBSD system you administer in place, not an
immutable image you rebuild. Its state lives in its own ZFS dataset:

```bash
jm ssh -- 'zfs list -o name,used | grep bastille'
```

```text
zroot/bastille/cache/15.1-RELEASE                                                   149M
zroot/bastille/jails/demo                                                          72.9M
zroot/bastille/jails/demo/root                                                      72.8M
zroot/bastille/releases/15.1-RELEASE                                                360M
```

Destroy it non-interactively with `-a -y` — `-f` alone still prompts, and a
prompt over `jm ssh --` fails with `[ERROR]: Invalid input. Please answer 'y'
or 'n'`:

```bash
jm ssh -- bastille destroy -a -y demo
jm ssh -- bastille list all
```

```text
[demo]:
Destroying jail...
Note: jail console logs archived.
/var/log/bastille/demo_console.log-2026-08-21

 JID  Boot  Prio  State  IP Address  Published Ports  Hostname  Release  Path
```

### They are the same primitive

Run `jls` in the guest with both kinds of thing running, and the point makes
itself:

```bash
jm ssh -- jls
```

```text
   JID  IP Address      Hostname                      Path
    13                  3447e34317d0                  /var/db/containers/storage/zfs/graph/9a88186abc12206eb…
    17  10.17.89.10     demo                          /usr/local/bastille/jails/demo/root
```

JID 13 is the registry **container**. JID 17 is the **jail**. Same kernel
mechanism, same isolation boundary — what differs is the tooling and the
lifecycle wrapped around it.

---

## Which one do you want?

| Choose a **container** when | Choose a **jail** when |
|---|---|
| The artefact is an image: built once, versioned, pushed, pulled, identical everywhere | The artefact is a **machine**: you install packages, edit `/usr/local/etc`, run `service` and keep it |
| It must also run on Linux, or in someone else's CI | It is FreeBSD-shaped anyway — `pf`, ZFS, `rc.d`, ports |
| You want a compose file or a Pod manifest to describe the whole stack | You want the thing FreeBSD servers actually run in production |
| You want the state thrown away on exit | You want the state kept, snapshotted, and rolled back with ZFS |
| You want ports published to the Mac — `-p` and `jm ports` handle it | You are happy reaching it from inside the guest over `bastille0` |
| You want the host's client tooling (`jpodman`, `jdocker`, compose) | You want `bastille` — templates, `bastille rdr`, `bastille zfs snapshot` |

Two practical limits that decide it for a lot of people:

- **A jail's IP is not reachable from the Mac.** The forwarder publishes
  *container* ports only. From inside the guest, `10.17.89.10` pings and
  serves; from the Mac, `curl` times out. Publishing a jail's port to the Mac
  means an `rdr` rule in `pf` plus a container-shaped hop, which the MVP does
  not automate.
- **A container gets no resource limits.** `--memory` and `--cpus` are
  accepted and silently ignored — there are no cgroups. `rctl` is present in
  the guest and is the FreeBSD way to limit a jail, but it needs
  `kern.racct.enable`, which this guest reports as `0` and which is a boot
  tunable; that route is **untested here**. Either way the VM's own
  `jm set --cpus` / `--memory` is the boundary you can rely on.

The honest summary: use containers for the things you build and ship, and a
jail when you want a small FreeBSD server you can log into. This machine runs
both, on the same kernel, at the same time.

---

## Cleanup

```bash
# jails
jm ssh -- bastille destroy -a -y demo
jm ssh -- bastille list all

# the registry, the images and the build directory
jpodman rm -f registry
jpodman rmi freebsd-hello:1.0 "$REG:5000/freebsd-hello:1.0"
jpodman rmi docker.io/library/registry:2 ghcr.io/freebsd/freebsd-runtime:15.1
rm -rf ~/jm-freebsd

# and, if you are finished with the machine altogether
jm stop
jm rm
```

Removing the bootstrapped release as well — 360 MiB in
`zroot/bastille/releases` — is `jm ssh -- bastille destroy -y 15.1-RELEASE`,
and it refuses until every jail cloned from it is gone:

```text
[ERROR]: (demo) depends on 15.1-RELEASE base.
[ERROR]: Cannot destroy base with child containers.
```

---

## Where next

| You want | Go to |
|---|---|
| The five published demo images, side by side | [demo/README.md](../../demo/README.md) |
| Everything the Linuxulator does not implement | [docs/LIMITATIONS.md](../LIMITATIONS.md#the-linuxulator) |
| Every command and flag | [docs/USAGE.md](../USAGE.md) |
| To start again from the first tutorial | [1. Your first container](01-first-container.md) |

---

*Output on this page was captured on 2026-08-21 against a machine built from
commit `8a20dda` — macOS 26.5 on Apple Silicon, guest FreeBSD 15.1-RELEASE-p2
arm64, guest podman 5.8.4, bastille 1.4.4. The three GHCR commands in part 2
are the only ones not run. Paths and the machine name have been shortened to
the defaults; the text of every other command and every message is verbatim.*
