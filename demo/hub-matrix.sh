#!/bin/sh
#
# demo/hub-matrix.sh -- run the Docker Hub compatibility matrix documented in
# docs/USAGE.md against a running jailmachine, and print it as a table.
#
#     jm start
#     demo/hub-matrix.sh
#
# Linux images are pulled and run with --os=linux (the guest is a FreeBSD
# host, so podman there defaults to freebsd/<arch>). FreeBSD images -- from
# Docker Hub and from GHCR alike -- need no flag.
#
# Two cases need a workaround and one does not work at all; the script checks
# all three explicitly rather than hiding them.
#
# Environment:
#   PODMAN    podman wrapper to use          (default: jpodman)
#   TIMEOUT   seconds allowed per container  (default: 120)
#   PORT_HTTP host port for the nginx check  (default: 18090)
#   PORT_REDIS host port for the redis check (default: 16390)

set -u

PODMAN=${PODMAN:-jpodman}
TIMEOUT=${TIMEOUT:-120}
PORT_HTTP=${PORT_HTTP:-18090}
PORT_REDIS=${PORT_REDIS:-16390}

pass=0
fail=0

# timeout(1) is GNU; on a stock macOS it arrives as gtimeout with coreutils.
if command -v timeout >/dev/null 2>&1; then
	TMO="timeout $TIMEOUT"
elif command -v gtimeout >/dev/null 2>&1; then
	TMO="gtimeout $TIMEOUT"
else
	TMO=""
	echo "note: no timeout(1) on PATH (brew install coreutils); running without one" >&2
fi

command -v "$PODMAN" >/dev/null 2>&1 || {
	echo "$PODMAN not found on PATH -- install jailmachine, or set PODMAN=" >&2
	exit 1
}

row() { printf '%-42s  %-8s  %-6s  %s\n' "$1" "$2" "$3" "$4"; }

verdict() { # verdict <rc> <output> <expected substring>
	if [ "$1" -ne 0 ]; then
		return 1
	fi
	case "$2" in
	*"$3"*) return 0 ;;
	*) return 1 ;;
	esac
}

# oneshot <image> <os: linux|freebsd> <expected substring> [command...]
oneshot() {
	img=$1
	os=$2
	want=$3
	shift 3
	if [ "$os" = linux ]; then osflag=--os=linux; else osflag=; fi

	# The exit status must come from the jpodman run itself, never from the
	# last stage of a pipeline -- hence the plain assignment and $? below.
	out=$($TMO "$PODMAN" run --rm $osflag "$img" "$@" 2>&1)
	rc=$?

	if verdict "$rc" "$out" "$want"; then
		pass=$((pass + 1))
		row "$img" "$os" "ok" "$(printf '%s' "$out" | head -n 1)"
	else
		fail=$((fail + 1))
		row "$img" "$os" "FAIL" "rc=$rc $(printf '%s' "$out" | head -n 1)"
	fi
}

cleanup() {
	$PODMAN rm -f hub-matrix-nginx hub-matrix-redis >/dev/null 2>&1
	:
}
trap 'cleanup' EXIT INT TERM
cleanup

echo "Docker Hub compatibility matrix -- $(date -u '+%Y-%m-%d %H:%M UTC')"
$PODMAN info --format 'guest: {{.Host.OS}}/{{.Host.Arch}}  podman {{.Version.Version}}' 2>/dev/null
echo
row IMAGE OS RESULT DETAIL
row "------------------------------------------" "--------" "------" "------"

# ------------------------------------------------ Linux images, no fuss ----
oneshot docker.io/library/alpine:latest linux Linux uname -sm
oneshot docker.io/library/debian:trixie-slim linux Linux uname -sm
oneshot docker.io/library/ubuntu:24.04 linux Linux uname -sm
oneshot docker.io/library/python:3-alpine linux Python python3 --version
oneshot docker.io/library/golang:alpine linux "go version" go version
oneshot docker.io/library/hello-world:latest linux Hello
oneshot docker.io/library/caddy:alpine linux v2 caddy version
oneshot docker.io/library/postgres:17-alpine linux postgres postgres --version

# --------------------------------------------- FreeBSD images, no flag ----
oneshot docker.io/dougrabson/freebsd15-minimal freebsd FreeBSD uname -so
oneshot docker.io/dougrabson/freebsd14-minimal freebsd FreeBSD uname -so
oneshot ghcr.io/freebsd/freebsd-runtime:15.1 freebsd 15.1-RELEASE freebsd-version
oneshot ghcr.io/freebsd/freebsd-runtime:14.3 freebsd 14.3-RELEASE freebsd-version

# ------------------------------------------------- redis: needs one flag ----
# redis-server aborts with "Failed to test the kernel for a bug that could
# lead to data corruption during background save"; its own documented switch
# skips that probe.
oneshot docker.io/library/redis:alpine linux "Redis server v" redis-server --version

$TMO "$PODMAN" run -d --name hub-matrix-redis --os=linux \
	-p "$PORT_REDIS":6379 docker.io/library/redis:alpine \
	redis-server --ignore-warnings ARM64-COW-BUG >/dev/null 2>&1
rc=$?
out=
if [ $rc -eq 0 ]; then
	i=0
	while [ $i -lt 15 ]; do
		out=$(printf 'PING\r\n' | nc 127.0.0.1 "$PORT_REDIS" 2>/dev/null)
		case "$out" in *PONG*) break ;; esac
		i=$((i + 1))
		sleep 1
	done
fi
if verdict "$rc" "$out" PONG; then
	pass=$((pass + 1))
	row "redis:alpine --ignore-warnings" linux "ok" "+PONG through 127.0.0.1:$PORT_REDIS"
else
	fail=$((fail + 1))
	row "redis:alpine --ignore-warnings" linux "FAIL" "rc=$rc no +PONG on 127.0.0.1:$PORT_REDIS"
fi
$PODMAN rm -f hub-matrix-redis >/dev/null 2>&1

# ------------------------------------------ nginx: needs one config line ----
# Stock nginx registers its listening socket with EPOLLEXCLUSIVE when
# worker_processes > 1; FreeBSD's linux_epoll returns EINVAL and every worker
# dies with "epoll_ctl(1, 6) failed (22: Invalid argument)". accept_mutex on
# takes the older, portable accept() path.
$TMO "$PODMAN" run -d --name hub-matrix-nginx --os=linux \
	-p "$PORT_HTTP":80 docker.io/library/nginx:1.31-alpine \
	sh -c 'sed -i "s/^events {/events {\n    accept_mutex on;/" /etc/nginx/nginx.conf && exec nginx -g "daemon off;"' \
	>/dev/null 2>&1
rc=$?
if [ $rc -eq 0 ]; then
	out=$(curl -fsS --retry 15 --retry-connrefused -o /dev/null \
		-w '%{http_code}' "http://127.0.0.1:$PORT_HTTP/" 2>&1)
else
	out=
fi
if verdict "$rc" "$out" 200; then
	pass=$((pass + 1))
	row "nginx:1.31-alpine + accept_mutex on" linux "ok" "HTTP 200 on 127.0.0.1:$PORT_HTTP"
else
	fail=$((fail + 1))
	row "nginx:1.31-alpine + accept_mutex on" linux "FAIL" "rc=$rc got '$out'"
fi
$PODMAN rm -f hub-matrix-nginx >/dev/null 2>&1

# ----------------------------------------------- node: known bad, no fix ----
# The binary starts and prints its version, but nothing written from
# JavaScript ever reaches the pipe and its sockets never accept. There is no
# known workaround in this release, so this row is expected to read "bad".
out=$($TMO "$PODMAN" run --rm --os=linux docker.io/library/node:22-alpine \
	node --version 2>&1)
ver=$(printf '%s' "$out" | head -n 1)
out=$($TMO "$PODMAN" run --rm --os=linux docker.io/library/node:22-alpine \
	node -e 'console.log("jm-node-check")' 2>&1)
rc=$?
if verdict "$rc" "$out" jm-node-check; then
	fail=$((fail + 1))
	row docker.io/library/node:22-alpine linux "?" "console.log now works -- update the docs"
else
	pass=$((pass + 1))
	row docker.io/library/node:22-alpine linux "bad" "$ver starts, console.log produces nothing (expected)"
fi

echo
echo "$pass as documented, $fail unexpected"
[ "$fail" -eq 0 ]
