#!/bin/sh
# jm-demo-volume -- where does a container's data actually live?
#
# Host bind-mounts do work on jailmachine: a shared directory appears in the
# guest at its own absolute path, so `-v /Users/you/src:/app` resolves. But
# the transport is virtio-9p and it is slow (~70 MB/s, and far worse on
# metadata). An engine-managed volume lives on the guest's ZFS pool instead,
# at native speed, and it survives the container. That is what this demo
# shows: the fast path for build output and databases.
set -u

B='\033[1m'; D='\033[2m'; C='\033[36m'; G='\033[32m'; Y='\033[33m'; RD='\033[31m'; R='\033[0m'
[ -t 1 ] || { B=''; D=''; C=''; G=''; Y=''; RD=''; R=''; }

DATA=${DATA_DIR:-/data}
LOG="$DATA/visits.log"

printf '\n'
printf "${B}┌────────────────────────────────────────────────────────────────────┐${R}\n"
printf "${B}│${R}  ${B}jailmachine demo${R} · ${C}where your container data lives${R}                ${B}│${R}\n"
printf "${B}└────────────────────────────────────────────────────────────────────┘${R}\n\n"

kv() { printf "  ${Y}%-16s${R} %b\n" "$1" "$2"; }

# --- is /data its own filesystem, or just a directory in the image layer? ---
if [ ! -d "$DATA" ]; then
    printf "  ${RD}%s does not exist.${R}\n\n" "$DATA"
    printf "  Run this image with a named volume:\n\n"
    printf "      ${G}jpodman run --rm -v jm-demo-data:%s ghcr.io/gabrielbelli/jm-demo-volume${R}\n\n" "$DATA"
    exit 0
fi

# shellcheck disable=SC2046
set -- $(df -h "$DATA" | sed -n 2p)
VFS=${1:-?}; VSIZE=${2:-?}; VUSED=${3:-?}; VAVAIL=${4:-?}
set -- $(df -h / | sed -n 2p)
RFS=${1:-?}

if [ "$VFS" = "$RFS" ]; then
    MOUNTED="${RD}no${R}  ${D}-- same filesystem as /, so this is just an image directory${R}"
    HINT="  ${D}Add ${R}${G}-v jm-demo-data:${DATA}${R}${D} to give it a real volume.${R}"
else
    MOUNTED="${G}yes${R} ${D}-- a separate filesystem, mounted into the jail${R}"
    HINT=""
fi

kv 'data dir'      "$DATA"
kv 'is a volume?'  "$MOUNTED"
kv 'guest path'    "${VFS}"
kv 'space'         "${VUSED} used of ${VSIZE}, ${VAVAIL} free"
kv 'physically on' "${D}the jailmachine guest's ZFS pool, inside the VM${R}"
[ -z "$HINT" ] || printf "\n%b\n" "$HINT"

# --- write, then show every previous write ---------------------------------
STAMP=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
if echo "$STAMP  run from container $(hostname)" >> "$LOG" 2>/dev/null; then
    N=$(grep -c '' "$LOG" 2>/dev/null || echo '?')
    case "$N" in 1) PLURAL=line ;; *) PLURAL=lines ;; esac
    printf "\n  ${B}Wrote one line to %s${R}  ${D}(now %s %s)${R}\n\n" "$LOG" "$N" "$PLURAL"
    sed -n '1,10p' "$LOG" | while read -r line; do printf "    ${D}%s${R}\n" "$line"; done
    [ "${N:-0}" -gt 10 ] 2>/dev/null && printf "    ${D}... and %s more${R}\n" "$(( N - 10 ))"
    printf "\n  ${D}Run this image again and the list grows. Delete the container --${R}\n"
    printf "  ${D}the data stays. That is the volume doing its job.${R}\n"
else
    printf "\n  ${RD}Could not write to %s (read-only?).${R}\n" "$LOG"
fi

# --- the honest caveat ------------------------------------------------------
printf "\n${B}%s${R}\n" 'Reaching this data from macOS'
printf "  ${D}%s${R}\n" 'This volume lives on the ZFS pool inside the VM, not on your Mac.'
printf "  ${D}%s${R}\n" 'A host directory would be reachable -- `-v /Users/you/src:/app`'
printf "  ${D}%s${R}\n" 'works, at its own path -- but over 9p, which is slow. To get a'
printf "  ${D}%s${R}\n" 'file out of the volume:'
printf "\n      ${G}%s${R}\n" 'jm ssh -- podman volume inspect jm-demo-data'
printf "      ${G}%s${R}\n" 'jpodman cp <container>:/data/visits.log ./visits.log'
printf "\n  ${D}%s${R}\n\n" 'Keep source on a share; keep build output and databases in a volume.'
