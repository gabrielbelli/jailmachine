<!-- linked from docs/tutorials/README.md -->
# 2. Develop on a shared folder

Keep your source tree on the Mac, run it inside a container, and understand
the three things that behave differently here: **file modes**, **file
watchers**, and **speed**.

| | |
|---|---|
| You need | A machine from [tutorial 1](01-first-container.md), started (`jm start`) |
| Time | About 10 minutes |
| You end with | A working edit-on-the-Mac / run-in-a-container loop, and a clear rule for when to stop using a share |

---

## The rule: host directories keep their own path

There is one rule, and it is the whole design (
[ADR 0007](../adr/0007-host-filesystem-sharing.md)): a shared host directory
appears **inside the guest at the same absolute path it has on the Mac**.
`jm` never rewrites a `-v` argument, and there is no `/host_mnt` prefix to
learn.

So both of these work, from any working directory:

```bash
jpodman run --rm --os=linux -v /Users/you/code:/Users/you/code docker.io/alpine ls /Users/you/code
jpodman run --rm --os=linux -v "$PWD:$PWD" -w "$PWD" docker.io/alpine ls
```

```text
app.js
```

A different target path works too — the rule is about the **source** side:

```bash
jpodman run --rm --os=linux -v "$PWD:/app" docker.io/alpine ls /app
```

The identity form (`-v "$PWD:$PWD" -w "$PWD"`) is worth the habit: the same
command line then means the same thing on this Mac, on a Linux box, and in
CI, and stack traces from inside the container name paths you can open in
your editor.

## What is shared, and how to change it

A new machine shares four roots, skipping any the host does not have:

| Root | Why |
|---|---|
| Your home directory | Where your work is |
| `/Volumes` | Where macOS mounts removable and network volumes |
| `/private/tmp` | The real location of `/tmp` |
| `$TMPDIR`'s parent (`/var/folders/<hash>`) | Where `mktemp -d` and every test harness put scratch directories |

```bash
jm inspect | grep '^Share'
```

```text
Share:             /Users/you (rw)
Share:             /Volumes (rw)
Share:             /private/tmp (rw)
Share:             /var/folders/5l/x_34hg657n93c50c02sbjb040000gn (rw)
```

> **Every container can read and write everything in that list** — by default
> your whole home directory, `~/.ssh` and `~/.aws` included, and
> `~/.jailmachine` itself, which holds the machine's private SSH key. It is
> the same posture as Docker Desktop's default, and it is deliberate. If you
> run images you do not trust, narrow it.

The share set changes only on a **stopped** machine, and applies at the next
start:

```bash
jm set --mount /work jailmachine
```

```text
jm: set jailmachine: jailmachine is running; cpus, memory, the ssh port and the shared directories change only on a stopped machine
  hint: stop the machine first: jm stop jailmachine
```

```bash
jm stop
jm set --no-mounts                                  # drop every share
jm set --mount ~/code --mount "/srv/data:ro"        # add back only these
jm start
```

`--no-mounts` cannot be combined with `--mount` or `--unmount` in one call,
hence the two commands:

```text
jm: set jailmachine: --no-mounts cannot be combined with --mount or --unmount
  hint: run 'jm set --help'
```

### `/tmp` is the one path that cannot follow the rule

On macOS `/tmp` is a symlink to `/private/tmp`, and a share mounted at the
guest's own `/tmp` would shadow it. So jm shares `/private/tmp` and leaves
your argument alone — which means you must write the real path:

```bash
jpodman run --rm --os=linux -v /private/tmp/demo:/app docker.io/alpine ls -A /app   # yes
jpodman run --rm --os=linux -v /tmp/demo:/app docker.io/alpine ls -A /app           # no
```

```text
marker
Error: preparing container 7aa1a48… for attach: ocijail: mounting
{"destination":"/app","source":"/tmp/demo","type":"bind"}: source path does not
exist: /tmp/demo (create the directory first): OCI runtime error
```

The second command asked for the **guest's** own `/tmp/demo`, which does not
exist. If a directory of that name happens to exist in the guest you get no
error at all — just an empty, wrong directory. `$TMPDIR` and `mktemp -d` hand
out `/var/folders/...` paths, which are shared, so those need no thought.

---

## The zsh trap: `:ro` is a history modifier

This one costs people an afternoon, so it gets its own section. In **zsh**,
a `:` after a parameter expansion introduces a *history modifier*. `:r`
means "strip the extension", and `:ro` parses as `:r` followed by a literal
`o`:

```bash
P=/Users/you/code
echo $P:ro            # zsh
```

```text
/Users/you/codeo
```

**Quoting does not save you.** Only braces (or escaping the colon) do:

| You write | zsh produces |
|---|---|
| `$P:ro` | `/Users/you/codeo` |
| `"$P:ro"` | `/Users/you/codeo` |
| `${P}:ro` | `/Users/you/code:ro` |
| `$P\:ro` | `/Users/you/code:ro` |

bash is unaffected — it produces `/Users/you/code:ro` in all four forms.

The damage is silent in both places it can happen:

```bash
jpodman run --rm --os=linux -v $P:$P:ro -w $P docker.io/alpine ls
```

```text
Error: preparing container 2e23e9db… for attach: workdir "/Users/you/code"
does not exist on container 2e23e9db…
```

— the mount went to `/Users/you/codeo`, so the working directory was not
there. And with `jm set`, a mangled path that happens to sit inside an
already-shared root is simply absorbed: **no error, no `:ro`, and the
directory stays read-write.** It exits `0`, and the only tell is what is
missing:

```console
$ jm set --mount $P:ro
==> jailmachine: 4 cpus, 4096 MiB, 64 GiB, ssh port 2222, publishing on 0.0.0.0
```

No `==> share:` lines, no "attached on the next start" notice. Compare that
with the correct form below.

Write it with braces, always:

```bash
jpodman run --rm --os=linux -v "${P}:${P}:ro" -w "${P}" docker.io/alpine ls
jm set --mount "${P}:ro"
```

```text
==> share: /Users/you/code (ro)
==> the shared directories are attached on the next start: jm start jailmachine
```

Read-only really is read-only from the guest:

```text
touch: nope: Read-only file system
```

---

## Modes and ownership: what `mapped-xattr` means

Shares are exported with the 9p **`mapped-xattr`** security model. Guest
ownership and modes are stored in host extended attributes instead of being
applied to the host file. This is a deliberate trade, and it goes in both
directions.

**Mac → container.** A file you create keeps its mode, and a non-root
container user can read it:

```bash
chmod 0755 app.js
jpodman run --rm --os=linux -v "$PWD:$PWD" -w "$PWD" docker.io/alpine ls -l app.js
jpodman run --rm --os=linux -u 1000:1000 -v "$PWD:$PWD" -w "$PWD" docker.io/alpine cat app.js
```

```text
-rwxr-xr-x    1 501      root            18 Aug 21 21:27 app.js
console.log("hi")
```

Your Mac uid (`501`) shows through unchanged.

**Container → Mac.** A file a container creates as root appears on the Mac as
`0600`, with its real mode and owner in `user.virtfs.*` xattrs:

```bash
jpodman run --rm --os=linux -v "$PWD:$PWD" -w "$PWD" docker.io/alpine \
  sh -c 'echo made-in-container > out.txt; chmod 0644 out.txt; ls -l out.txt'
ls -l@ out.txt
xattr -px user.virtfs.mode out.txt
cat out.txt
```

```text
-rw-r--r--    1 root     root            18 Aug 21 21:27 out.txt
-rw-------@ 1 you  staff  18 21 Aug 18:27 out.txt
	user.virtfs.mode	 2
	user.virtfs.uid	 4
A4 81
made-in-container
```

`A4 81` is little-endian `0x81A4` — `S_IFREG | 0644`, exactly what the
container set. The file is still perfectly readable and writable by you; only
`ls -l` lies.

Summarised:

| Created by | Mode in the container | Mode on the Mac |
|---|---|---|
| The Mac, `0755` | `-rwxr-xr-x`, owned by uid `501` | `-rwxr-xr-x`, exactly as written |
| A container running as root, `0644` | `-rw-r--r--`, owned by uid `0` | `-rw-------`, real mode in `user.virtfs.mode` |

**Why put up with that?** The alternative, `security_model=none`, applies the
guest's modes to the host file — and the host end of a share runs as your
unprivileged Mac user, so a container running as root could not write a file
it had just made read-only. macOS enforces the mode even for the owner. Git
does exactly that with its pack temp files, so under `none` a clone into a
shared directory fails outright:

```text
fatal: Unable to create temporary file '.../.git/objects/pack/tmp_pack_XXXXXX': Permission denied
```

Under `mapped-xattr` the same clone succeeds. If you would rather have
host-native modes and no root-in-a-container writes,
`JM_9P_SECURITY=none jm start` trades it back — it is read at every start and
not stored on the machine.

> **One related no-op**: `utimes` from inside the guest does not stick. A
> `touch -t 200001010000` **on the Mac** sets the timestamp normally; the same
> command **in a container** silently leaves the file at the current time. Do
> not build anything that depends on setting an mtime from inside a container
> on a share.

---

## File watchers: `inotify` never fires — poll instead

This is the biggest behavioural difference, and it is not subtle. On a share
the watch **cannot even be created**:

```bash
jpodman run --rm --os=linux -v "$PWD:$PWD" -v myvol:/vol docker.io/alpine sh -c '
  apk add --no-cache inotify-tools >/dev/null 2>&1
  inotifywait -t 2 '"$PWD"'; echo "exit=$?"
  inotifywait -t 2 /vol;     echo "exit=$?"'
```

```text
Setting up watches.
Couldn't watch /Users/you/code: Bad file descriptor
exit=1
Setting up watches.
Watches established.
exit=2
```

The control on a named volume works, and so does a watch on the container's
own filesystem. It is `p9fs` specifically that the Linuxulator's `inotify`
cannot watch — tracked as
[#4](https://github.com/gabrielbelli/jailmachine/issues/4).

**Reads are perfectly coherent**, though, which is what makes polling work.
A container polling `stat` sees a host-side write within its poll interval,
with an updated mtime:

```bash
echo v1 > f.txt
jpodman run -d --rm --os=linux --name poller -v "$PWD:$PWD" -w "$PWD" docker.io/alpine \
  sh -c 'while true; do echo "$(date +%T) $(stat -c %Y f.txt) $(cat f.txt)"; sleep 1; done'
sleep 2; echo v2 > f.txt
sleep 2; echo v3 > f.txt
sleep 2; jpodman logs poller; jpodman rm -f poller
```

```text
21:29:43 1787347781 v1
21:29:44 1787347781 v1
21:29:45 1787347785 v2
21:29:46 1787347785 v2
21:29:47 1787347787 v3
21:29:48 1787347787 v3
```

So the rule is: **turn on your tool's polling watcher**. These are the
upstream tools' own switches, not jm flags:

| Tool | Turn on polling with |
|---|---|
| chokidar, and everything built on it (webpack dev server, gulp, Docusaurus) | `CHOKIDAR_USEPOLLING=1`, interval via `CHOKIDAR_INTERVAL=500` |
| nodemon | `--legacy-watch` (`-L`) |
| Vite dev server, and Vitest (same watcher) | `server: { watch: { usePolling: true } }` in `vite.config.*` |
| webpack (directly) | `watchOptions: { poll: 1000 }` |
| Tailwind CLI | `--poll` |
| watchexec, and `cargo watch` through it | `--poll 500ms` |
| Air (Go live reload) | `poll = true` under `[build]` in `.air.toml` |
| Django `runserver` | Nothing — its default `StatReloader` already polls |

If a tool has no polling mode at all (Jest's watcher is the common one), keep
the watched tree in a **named volume** and edit through the container, or run
the watcher on the Mac and use the container only to execute.

Polling costs CPU in proportion to the tree size. `node_modules` in a poll
loop over 9p is the pathological case — see the next section for the fix.

---

## Speed, and when to stop using a share

Measured on this machine (4 vCPU / 4096 MiB, a **busy** Mac), the same
operations on a 9p share and in an engine-managed volume on the guest's ZFS:

| Operation | 9p share | Named volume (ZFS) | Ratio |
|---|---|---|---|
| Write 200 MiB, `dd conv=fsync` | 2.66 s — **75.1 MB/s** | 0.29 s — 683 MB/s | 9× |
| Read 200 MiB | 2.86 s — **69.9 MB/s** | 0.04 s — 4.3 GB/s (page cache) | — |
| Create 1000 × 4 KiB files | **4.10 s** | 0.32 s | 13× |
| `git clone --depth 1` of this repo | **4.45 s** | 0.94 s | 5× |

Metadata is the expensive part, not bandwidth. A big sequential write is
merely slow; ten thousand small file creations — which is what `npm install`,
`cargo build` and `go build` do — is where an hour goes.

For the cross-stack picture, [docs/COMPARISON.md](../COMPARISON.md#filesystem-sharing)
measured the same clone at 0.26 s under Docker Desktop's virtiofs, against
4.72 s here. There is no faster transport to switch to: FreeBSD has no
virtiofs driver at all, so 9p is the only option with a driver on both ends.

**The working pattern is to split the tree**: source on the share, build
output in a volume.

Mount a named volume **over a subdirectory of the share**. The engine
resolves the deeper mount last, so the source tree stays editable on the Mac
while the heavy directory lives on ZFS:

```bash
jpodman volume create build-cache

jpodman run --rm --os=linux \
  -v "$PWD:$PWD" -w "$PWD" \
  -v build-cache:"$PWD/vendor" \
  docker.io/alpine sh -c '
    i=0; while [ $i -lt 500 ]; do : > vendor/f$i; i=$((i+1)); done   # in the volume
    mkdir -p src
    i=0; while [ $i -lt 500 ]; do : > src/f$i;    i=$((i+1)); done   # on the share
  '
```

500 empty files took **0.36 s** in `vendor/` and **1.69 s** in `src/`. On the
Mac, `src/` has the 500 files and `vendor/` is empty — its contents are inside
the VM, which is exactly what you want for a directory you never open in your
editor.

| Put this on a share | Put this in a named volume |
|---|---|
| Source files you edit in your editor | `node_modules`, `target/`, `vendor/`, `.venv` |
| Config, fixtures, small data files | Build caches, image layers, compiler output |
| Anything you want to `git status` on the Mac | Database data directories |
| | Anything a watcher needs to see change from **inside** the container |

`jm ssh -- ls /var/db/containers/storage/volumes/<name>/_data` is where a
named volume actually lives — on the guest's ZFS pool, inside the VM. That is
why it is fast, and also why the Mac cannot open it directly.

> `docker.io/node:22-alpine` **hangs** under the Linuxulator — `node --version`
> prints, but `node -e ''` never returns. Use `node:22-bookworm-slim`, which
> is verified working. See [docs/LIMITATIONS.md](../LIMITATIONS.md#the-linuxulator).

---

## Cleanup

```bash
jpodman rm -f poller 2>/dev/null
jpodman volume rm build-cache myvol 2>/dev/null
rm -f out.txt f.txt

# if you narrowed or widened the share set while reading this:
jm stop
jm set --unmount ~/code           # drop whatever you added
jm start
jm inspect | grep '^Share'
```

There is no "restore the defaults" command — the default set is chosen at
`jm init`, so if you have lost track of it, `jm rm && jm init` is the reliable
way back. Nothing in a shared host directory is touched by either.

Removing the machine entirely (`jm rm`) never touches a shared host
directory — shares are exports, not copies.

---

## Where next

| You want | Go to |
|---|---|
| Two containers that talk to each other | [3. A stack with compose and kube](03-a-stack-with-compose-and-kube.md) |
| Containers with no Linux in them at all | [4. FreeBSD images and jails](04-freebsd-images-and-jails.md) |
| The full list of 9p gaps | [docs/LIMITATIONS.md](../LIMITATIONS.md#filesystem-sharing) |
| A share is empty and you do not know why | [docs/TROUBLESHOOTING.md](../TROUBLESHOOTING.md#a-share-is-empty-or-a--v-mounts-nothing) |

---

*Output on this page was captured on 2026-08-21 against a machine built from
commit `8a20dda` — macOS 26.5 on Apple Silicon, guest FreeBSD 15.1-RELEASE-p2
arm64, guest podman 5.8.4. Timings were taken on a loaded host and are
pessimistic. Paths and the machine name have been shortened to the defaults;
the text of every command and every message is verbatim.*
