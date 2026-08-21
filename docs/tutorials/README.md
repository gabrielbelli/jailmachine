<!-- linked from README.md -->
# Tutorials

Four self-contained walkthroughs. Each starts from a working `jm start`, ends
with a cleanup step, and shows the real output of the commands that matter.

| # | Tutorial | What it covers |
|---|---|---|
| 1 | [Your first container](01-first-container.md) | Install, `jm init`, `jm start`, a Linux image and a native FreeBSD one, a published port, `jm ssh`, `jm doctor`, `jm stop` |
| 2 | [Develop on a shared folder](02-develop-on-a-shared-folder.md) | The identity-path share, file modes under `mapped-xattr`, why watchers must poll, and when to use a named volume instead |
| 3 | [A stack with compose and kube](03-a-stack-with-compose-and-kube.md) | One two-service stack twice — `jdocker compose` and `jpodman kube play` — plus platform rules, service-name DNS, and the lifecycle caveats |
| 4 | [FreeBSD images and jails](04-freebsd-images-and-jails.md) | Build and push a native FreeBSD image, then create a bastille jail, and choose between them |

## Which one do I want?

| If you are… | Read | Because |
|---|---|---|
| New to jailmachine | **1** | Nothing else assumes anything, and it is the only one that installs anything |
| Moving a project's dev loop onto it | **1**, then **2** | The share is where the surprises are: no `inotify`, `0600` on the Mac, 9p speed |
| Porting a `compose.yaml` or a Kubernetes manifest | **3** | Platform naming differs per route, and container names do not resolve |
| Deciding whether the whole idea fits your project | **3** and **4** | Between them they name every gap you would hit in week one |
| Interested in FreeBSD itself, not in Docker parity | **4** | Native images and jails, and why a container here *is* a jail |
| Debugging something right now | none of them | `jm doctor`, then [docs/TROUBLESHOOTING.md](../TROUBLESHOOTING.md) |

## Related pages

| Page | What it is |
|---|---|
| [docs/INSTALL.md](../INSTALL.md) | Install paths, requirements, uninstalling |
| [docs/USAGE.md](../USAGE.md) | Every command, flag and environment variable |
| [docs/TROUBLESHOOTING.md](../TROUBLESHOOTING.md) | Symptom → cause → fix |
| [docs/LIMITATIONS.md](../LIMITATIONS.md) | Everything jailmachine cannot do, and whose limitation each one is |
| [docs/COMPARISON.md](../COMPARISON.md) | Head to head against Docker Desktop and podman machine |
| [demo/README.md](../../demo/README.md) | Five published demo images, and a five-minute tour |

## About the output on these pages

Every command was run before it was written down, on 2026-08-21, against a
machine built from commit `8a20dda`: macOS 26.5 on Apple Silicon, guest
FreeBSD 15.1-RELEASE-p2 arm64, guest podman 5.8.4, host podman 6.1.0.

Output blocks are real and trimmed. Paths, machine names and project
directories are shortened to the defaults (`/Users/you/...`, `jailmachine`) so
the pages read as one story; the text of every command and every message is
verbatim. Three commands are marked as not run — the GHCR push in tutorial 4,
which would publish a package under a real account.

Timings were taken on a **loaded** host — another VM and a full Docker Desktop
running throughout — so they are pessimistic. Where a page quotes a faster
figure for an idle Mac, it says so.
