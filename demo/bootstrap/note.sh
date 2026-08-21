#!/bin/sh
# Placeholder entrypoint. Never tagged :latest.
cat <<'TXT'
This is the :bootstrap tag of a jailmachine demo image.

It exists so that GitHub Actions creates the package with GITHUB_TOKEN, which
is the only way a ghcr.io package gets connected to its repository (and so
inherits the repository's public visibility). The real image is built on a Mac
with a running jailmachine, because it needs a FreeBSD userland to run pkg(8):

    make -C demo push

Pull :latest for the real thing.
TXT
