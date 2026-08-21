#!/bin/sh
# Write the runtime facts the demo page links to, then hand over to nginx.
# Doing this at start (not at build) keeps the output honest wherever the
# image was built -- CI on Linux, or locally on the jailmachine guest.
set -eu
umask 022          # the container inherits umask 077; nginx runs as `nginx`
: "${DOCROOT:=/usr/share/nginx/html}"

{
  echo "flavour         : Linux userland (Linuxulator)"
  echo "uname -s        : $(uname -s)"
  echo "uname -r        : $(uname -r)      # faked by compat.linux.osrelease"
  echo "uname -m        : $(uname -m)"
  echo "uname -a        : $(uname -a)"
  echo "/proc/version   : $(cat /proc/version 2>/dev/null || echo n/a)"
  echo "distro          : $(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-unknown}")"
  echo "libc            : $(ldd --version 2>&1 | sed -n 1p)"
  echo "nginx           : $(nginx -v 2>&1)"
  echo "root fs         : $(sed -n '1s/^[^ ]* [^ ]* \([^ ]*\).*/\1/p' /proc/mounts 2>/dev/null)"
  echo "started         : $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
} > "$DOCROOT/whoami.txt"

exec nginx -g 'daemon off;'
