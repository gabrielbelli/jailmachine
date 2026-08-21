#!/bin/sh
# Write the runtime facts the demo page links to, then hand over to nginx.
set -eu
umask 022
: "${DOCROOT:=/usr/local/www/nginx}"

s() { sysctl -n "$1" 2>/dev/null || echo n/a; }

{
  echo "flavour         : Native FreeBSD"
  echo "uname -s        : $(uname -s)"
  echo "uname -r        : $(uname -r)      # the real kernel, no fiction"
  echo "uname -m        : $(uname -m)"
  echo "uname -a        : $(uname -a)"
  echo "userland        : $(freebsd-version -u 2>/dev/null || echo n/a)"
  echo "ABI             : $(s hw.machine_arch)"
  echo "jailed          : $(s security.jail.jailed)   # 1 = this container is a jail"
  echo "event method    : kqueue"
  echo "nginx           : $(nginx -v 2>&1)"
  echo "root fs         : $(df -T / 2>/dev/null | sed -n '2s/^[^ ]* *\([^ ]*\).*/\1/p' || echo zfs)"
  echo "started         : $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
} > "$DOCROOT/whoami.txt"

exec nginx -g 'daemon off;'
