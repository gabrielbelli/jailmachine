<!-- linked from docs/tutorials/README.md -->
# 3. A stack with compose, and the same stack as a Pod

Run one two-service stack — a web server and a redis — twice: once from a
`compose.yaml` through `jdocker compose`, and once from a Kubernetes `Pod`
manifest through `jpodman kube play`. Along the way, the two rules that catch
everybody: **how each route names a platform**, and **why service names do not
resolve**.

| | |
|---|---|
| You need | A machine from [tutorial 1](01-first-container.md), started. For the compose half, the docker CLI (`brew install docker`) |
| Time | About 15 minutes |
| You end with | The same stack running two different ways, and torn down |

---

## Three routes, one engine

| Route | Needs on the Mac | Pick it when |
|---|---|---|
| `jdocker compose` | the `docker` CLI with the Compose plugin | You already have a `compose.yaml` and want Docker Desktop's behaviour |
| `jpodman compose` | `podman`, plus Compose as its external provider | podman is your client and you would rather not point `DOCKER_HOST` yourself |
| `jpodman kube play` | nothing but `podman` | You want the route FreeBSD implements itself, and the same YAML a FreeBSD server would run |

All three end at the **same engine** — the guest's podman — and every port
they publish goes through the **same forwarder**, so `jm ports` explains any
of them.

> **Nothing compose-shaped is installed in the guest — by choice.**
> `py312-podman-compose` 1.5.0 *is* packaged for FreeBSD
> (`jm ssh -- pkg search -e py312-podman-compose` will show it), but jm does
> not install it: our compose story is host-side. `podman compose` is a shim
> that hands the work to a provider running **on your Mac**, and `jdocker
> compose` uses the Compose plugin you already have. `podman kube play` is
> the one route podman implements itself, which is why it is the
> FreeBSD-native answer.

---

## Part 1 — the stack as a compose file

Make a directory with something to serve:

```bash
mkdir -p ~/jm-stack/www && cd ~/jm-stack
echo '<h1>hello from a compose stack</h1>' > www/index.html
```

### The first attempt, and why it fails

The obvious file is this, and it starts cleanly:

```yaml
# compose.yaml — this version does NOT work
services:
  web:
    image: docker.io/library/busybox
    platform: linux/arm64
    command: ["httpd", "-f", "-p", "80", "-h", "/www"]
    volumes:
      - ./www:/www:ro
    ports:
      - "8190:80"
  redis:
    image: docker.io/library/redis:alpine
    platform: linux/arm64
    command: ["redis-server", "--ignore-warnings", "ARM64-COW-BUG"]
```

Both containers come up. Then `web` tries to reach `redis`:

```bash
jdocker compose exec -T web sh -c 'printf "PING\r\n" | nc redis 6379'
```

```text
nc: bad address 'redis'
```

**Container-name DNS does not work here**, and this is the single thing most
likely to break a compose file you bring from elsewhere. The engine in the
guest uses the **CNI** network backend, because `netavark` — the backend that
carries container DNS on Linux — is not packaged for FreeBSD. Nor is the CNI
`dnsname` plugin, which is the other way CNI could answer names.
`aardvark-dns` 2.0.0 *is* in the FreeBSD package repo, but it is netavark's
DNS server and does nothing without it. So every network reports DNS off:

```bash
jpodman network inspect jm-stack_default --format '{{.DNSEnabled}}'
```

```text
false
```

The container's own view confirms it. Its only nameserver is the machine's,
and its `/etc/hosts` names itself and the Mac — nothing else in the stack:

```text
search mygateway
nameserver 192.168.127.2
---
127.0.0.1	localhost
::1	localhost
192.168.127.254	host.containers.internal host.docker.internal
10.89.2.3	162d1d0bee6c jm-stack-web-1
```

Everything the *host* can resolve still resolves inside the container —
`/etc/hosts` entries, `.local` names, VPN records, `host.docker.internal`.
It is only **container-to-container** names that have no answer.

This is a podman-on-FreeBSD gap, not one of jm's: a bare-metal FreeBSD
container host behaves the same. It is tracked as
[#5](https://github.com/gabrielbelli/jailmachine/issues/5), which also
sketches the fix — jm already owns the guest's resolver, so the forwarder
could push `name -> IP` records into the guest's `local_unbound` as
containers come and go.

Three things work today. This tutorial uses the first two:

| Workaround | Shape |
|---|---|
| One shared network namespace, in compose | `network_mode: "service:<name>"` — the next section |
| A **Pod** | Pod mates share a namespace, so `127.0.0.1` reaches them — [part 2](#part-2--the-same-stack-as-a-pod-manifest). Their *container* names land in each other's `/etc/hosts` too, but under `kube play` that is `<pod>-<container>`, not the manifest's `name:` |
| Static entries | `extra_hosts:` in compose, `--add-host name:IP` on `podman run`, with the sibling's bridge address from `jpodman inspect <name> --format '{{.NetworkSettings.IPAddress}}'`. It works, but the address changes when the container is re-created |

### The fix: share one network namespace

Put the services in a **single network namespace** with
`network_mode: "service:<name>"`. They then reach each other on `127.0.0.1`,
exactly as containers in a Kubernetes pod do — which is also what the second
half of this tutorial builds:

```yaml
# compose.yaml
services:
  web:
    image: docker.io/library/busybox
    platform: linux/arm64
    command: ["httpd", "-f", "-p", "80", "-h", "/www"]
    volumes:
      - ./www:/www:ro
    ports:
      - "8190:80"

  redis:
    image: docker.io/library/redis:alpine
    platform: linux/arm64
    command: ["redis-server", "--ignore-warnings", "ARM64-COW-BUG"]
    network_mode: "service:web"
    depends_on:
      - web
```

Two details in that file that are not about DNS:

- `platform: linux/arm64` on both services — see [Platform, per route](#platform-per-route).
- `--ignore-warnings ARM64-COW-BUG` on redis. Redis' own startup probe for an
  ARM64 copy-on-write kernel bug cannot run here and Redis exits rather than
  guess. The switch is Redis', not ours.

```bash
jdocker compose up -d
```

```text
 Container jm-stack-web-1 Creating
 Container jm-stack-web-1 Created
 Container jm-stack-redis-1 Creating
 Container jm-stack-redis-1 Created
 Container jm-stack-web-1 Starting
 Container jm-stack-web-1 Started
 Container jm-stack-redis-1 Starting
 Container jm-stack-redis-1 Started
```

```bash
jdocker compose ps
curl -s --retry 10 --retry-connrefused http://localhost:8190/
jdocker compose exec -T web sh -c 'printf "PING\r\n" | nc 127.0.0.1 6379'
```

```text
NAME              IMAGE                              COMMAND                  SERVICE   STATUS         PORTS
jm-stack-redis-1  docker.io/library/redis:alpine     "redis-server --igno…"   redis     Up 2 seconds   0.0.0.0:8190->80/tcp
jm-stack-web-1    docker.io/library/busybox:latest   "httpd -f -p 80 -h /…"   web       Up 2 seconds   0.0.0.0:8190->80/tcp
<h1>hello from a compose stack</h1>
+PONG
```

Both containers list the same published port because they share the one
network namespace. The bind mount `./www:/www:ro` needed nothing special:
Compose expands the relative source to an absolute host path, and that path
exists in the guest **unchanged** (see [tutorial 2](02-develop-on-a-shared-folder.md)).

Leave it running for now.

---

## Platform, per route

The guest is a FreeBSD host, so podman there selects **FreeBSD image
variants** by default. Neither compose nor `kube play` has an `--os` flag, so
the choice has to be made another way — and the answer differs per route.

| Route | Say it like this |
|---|---|
| `jdocker compose` | Usually nothing: the wrapper defaults `DOCKER_DEFAULT_PLATFORM=linux/arm64`. `platform: linux/arm64` on the service is the explicit, portable spelling |
| `jpodman compose` | `platform: linux/arm64` on the service — the podman wrapper sets **no** default platform |
| `eval "$(jm env)"; docker compose` | `platform: linux/arm64`, or pre-pull plus `pull_policy: missing` |
| `jpodman kube play` | `jpodman pull --os=linux <image>` first; the manifest cannot ask |

The difference is easy to see. Given a service with no `platform:` line:

```yaml
services:
  caddy:
    image: docker.io/library/caddy:alpine
```

```bash
jpodman compose up -d
```

```text
 Image docker.io/library/caddy:alpine Pulling
 Image docker.io/library/caddy:alpine Error no image found in image index for architecture "arm64", variant "", OS "freebsd"
Error response from daemon: no image found in image index for architecture "arm64", variant "", OS "freebsd"
Error: executing /usr/local/bin/docker-compose up -d: exit status 1
```

```bash
jdocker compose up -d
```

```text
 Container caddy-caddy-1 Started
```

Same file, same engine — the wrapper's default platform is the only
difference. Native FreeBSD images are the mirror image: they need nothing
under `jpodman`, and need `DOCKER_DEFAULT_PLATFORM=freebsd/arm64` or an
explicit `platform:` under `jdocker`.

---

## Part 2 — the same stack as a Pod manifest

`podman kube play` reads a Kubernetes `Pod` (or `Deployment`) manifest and
runs it, with no provider on the Mac and no compose implementation anywhere.
A pod gives you the shared network namespace **for free**: `network_mode` was
a way of hand-building what a pod is.

Take the compose stack down first, so the two do not fight over port 8190:

```bash
jdocker compose down
```

```yaml
# pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: jm-stack
spec:
  containers:
    - name: web
      image: docker.io/library/busybox
      imagePullPolicy: IfNotPresent
      command: ["httpd", "-f", "-p", "80", "-h", "/www"]
      volumeMounts:
        - name: www
          mountPath: /www
          readOnly: true
      ports:
        - containerPort: 80
          hostPort: 8191
    - name: redis
      image: docker.io/library/redis:alpine
      imagePullPolicy: IfNotPresent
      command: ["redis-server", "--ignore-warnings", "ARM64-COW-BUG"]
  volumes:
    - name: www
      hostPath:
        path: /Users/you/jm-stack/www      # an absolute host path — the same one in the guest
        type: Directory
```

The manifest cannot ask for a platform, so put the Linux images in the guest
first, then play it:

```bash
jpodman pull --os=linux docker.io/library/busybox docker.io/library/redis:alpine
jpodman kube play pod.yaml
```

```text
Pod:
61de5f3724015a0fcc80d27f52124411fe853883f49a6197469e5419bf7548e7
Containers:
5a05dac5d4a4bb76f8353d83ced84460ee25e8cc7df33c3eac868d6cb3175f6d
4e1d06cd3b68ebeef87fa1a70f1ede8f53610dee841c94504bab468cc4540dda
```

```bash
curl -s --retry 10 --retry-connrefused http://localhost:8191/
jpodman exec jm-stack-web sh -c 'printf "PING\r\n" | nc 127.0.0.1 6379'
jpodman pod ps
```

```text
<h1>hello from a compose stack</h1>
+PONG
POD ID        NAME        STATUS      CREATED       INFRA ID      # OF CONTAINERS
61de5f372401  jm-stack    Running     1 second ago  0ebb903e97a8  3
```

Three containers, not two: a pod also has an **infra container**, whose init
process is `catatonit`. The guest image installs it alongside `podman-suite`,
so `kube play` works out of the box on a machine created from a current image.

Note what the pod bought you: `nc 127.0.0.1 6379` reached redis from the web
container with no `network_mode` gymnastics. Podman also writes each pod
mate's **container name** into the others' `/etc/hosts` — but under `kube
play` that name is `<pod>-<container>`, not the manifest's bare `name:`:

```bash
jpodman exec jm-stack-web cat /etc/hosts
```

```text
::1	localhost localhost.my.domain
127.0.0.1	localhost localhost.my.domain
192.168.127.254	host.containers.internal host.docker.internal
10.89.0.3	jm-stack 2771defc653b-infra
127.0.0.1	jm-stack-web
127.0.0.1	jm-stack-redis
```

So `nc jm-stack-redis 6379` answers and `nc redis 6379` still says `bad
address`. **Use `127.0.0.1` inside a pod** — it is the one spelling that does
not depend on how the containers were named.

### `imagePullPolicy: IfNotPresent`

Kubernetes' rule is that an image tagged `:latest`, or not tagged at all, is
pulled **every time**. That pull would ask the registry for the FreeBSD
variant again and undo the `--os=linux` pull you just did. `IfNotPresent`
uses what is already in the guest.

We could not reproduce that here. Omitting `imagePullPolicy` entirely with
podman 5.8.4 in the guest did **not** re-pull: it reused the local
`linux/arm64` image and only warned,
`WARNING: image platform (linux/arm64/v8) does not match the expected platform (freebsd/arm64)`.
So state the observed behaviour, not the rule — and write `IfNotPresent`
anyway, because it is explicit, costs nothing, and does not depend on a
pull-policy default that may differ between podman versions.

> `jpodman kube generate <pod|container>` writes a manifest out of what is
> already running, which is the quickest way to a file that works.

---

## Ports: both routes, one forwarder

A compose `ports:` entry and a `hostPort:` in a manifest are both just
published ports on the guest's engine. The detached forwarder watches the
guest's container state and converges gvproxy's mapping table onto it:

```bash
jm ports
```

```text
# publishing on 0.0.0.0 unless -p names a host address
LOCAL         REMOTE              PROTO  STATUS
0.0.0.0:8191  192.168.127.2:8191  tcp    ok
```

Which means the same rules apply as for `jpodman run -p`:

| | |
|---|---|
| Default host address | the machine's publish address — `0.0.0.0`, every interface, **including your LAN**. `jm set --publish-addr 127.0.0.1` changes it |
| Timing | the mapping appears a second or two after the container starts, hence `curl --retry --retry-connrefused` everywhere in these pages |
| Failure | reported **per mapping** in `jm ports`, not by failing the stack. A host port already taken shows as `error: another process on this Mac already holds this host port (lsof -nP -iTCP:<port>)` |

---

## The lifecycle caveat: healthchecks never fire

Anything long-lived started from a compose file or a manifest is affected by
this, so read it before you design around either.

**Healthchecks do not run on a schedule.** podman schedules them with systemd
transient timers, and there is no systemd here:

```bash
jpodman run -d --os=linux --name hc --health-cmd 'true' --health-interval 2s docker.io/alpine sleep 60
sleep 14
jpodman inspect hc --format '{{json .State.Health}}'
```

```text
{"Status":"starting","FailingStreak":0,"Log":null}
```

Fourteen seconds, a two-second interval, **zero log entries** — the timer
never ran. Run one by hand and it answers immediately:

```bash
jm ssh -- podman healthcheck run hc
jpodman inspect hc --format '{{.State.Health.Status}}'
```

```text
healthy
```

So a `healthcheck:` block or a `livenessProbe` is inert: it will sit at
`starting` for ever. If you need one, drive it from a cron entry in the guest.
This is a podman-on-FreeBSD gap — a bare-metal FreeBSD container host behaves
the same — tracked as
[#3](https://github.com/gabrielbelli/jailmachine/issues/3).

**Restart policies, on the other hand, work.** This was measured twice
because the shipped docs said otherwise:

```bash
jpodman run -d --os=linux --restart=always --name rs docker.io/alpine sh -c 'sleep 2; exit 3'
for i in 1 2 3 4 5; do sleep 5
  printf '%2ds RestartCount=%s Status=%s\n' $((i*5)) \
    "$(jpodman inspect rs --format '{{.RestartCount}}')" \
    "$(jpodman inspect rs --format '{{.State.Status}}')"
done
```

```text
 5s RestartCount=2 Status=running
10s RestartCount=4 Status=running
15s RestartCount=6 Status=running
20s RestartCount=9 Status=running
25s RestartCount=11 Status=running
```

`restart: always` and `restart: on-failure` in a compose file both do what you
expect. Only the *health* half of #3 is real.

> Two other lifecycle flags are worse than broken — they are **silently
> ignored**. `--memory` and `--cpus` (and `mem_limit`/`cpus` in compose) are
> accepted, recorded as `0`, and enforce nothing: there are no cgroups here.
> A hardened compose file ports across unchanged and runs with no limits at
> all. The VM's own `jm set --cpus` / `--memory` is the only resource boundary
> you have. See [docs/LIMITATIONS.md](../LIMITATIONS.md#container-lifecycle).

---

## Cleanup

```bash
jpodman kube down pod.yaml
jdocker compose down                # if the compose stack is still up
jpodman rm -f hc rs 2>/dev/null
jpodman ps -a                       # empty
jpodman pod ps                      # empty
rm -rf ~/jm-stack
```

```text
Pods removed:
Secrets removed:
Volumes removed:
CONTAINER ID  IMAGE       COMMAND     CREATED     STATUS      PORTS       NAMES
POD ID      NAME        STATUS      CREATED     INFRA ID    # OF CONTAINERS
```

---

## Where next

| You want | Go to |
|---|---|
| Containers with no Linux in them at all, and jails | [4. FreeBSD images and jails](04-freebsd-images-and-jails.md) |
| The three compose routes in full | [docs/USAGE.md](../USAGE.md#compose-and-kubernetes-yaml) |
| Everything that is accepted and then ignored | [docs/LIMITATIONS.md](../LIMITATIONS.md#container-lifecycle) |

---

*Output on this page was captured on 2026-08-21 against a machine built from
commit `8a20dda` — macOS 26.5 on Apple Silicon, guest FreeBSD 15.1-RELEASE-p2
arm64, guest podman 5.8.4, host podman 6.1.0, docker CLI 29.6.2 with Compose
v5.3.1. Paths, project name and the machine name have been shortened to the
defaults; the text of every command and every message is verbatim.*
