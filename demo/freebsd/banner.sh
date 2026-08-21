#!/bin/sh
# jm-demo-freebsd -- prove that this container's kernel really is FreeBSD.
#
# Deliberately written against the *runtime* image's tool set only:
# freebsd-runtime has no awk, no head, no wc. sh builtins + sysctl are enough.
set -u

B='\033[1m'; D='\033[2m'; C='\033[36m'; G='\033[32m'; Y='\033[33m'; R='\033[0m'
[ -t 1 ] || { B=''; D=''; C=''; G=''; Y=''; R=''; }

s() { sysctl -n "$1" 2>/dev/null || echo '(unavailable)'; }

# ---- facts -----------------------------------------------------------------
OSTYPE=$(s kern.ostype)
OSRELEASE=$(s kern.osrelease)
OSRELDATE=$(s kern.osreldate)
ARCH=$(s hw.machine_arch)
MACHINE=$(s hw.machine)
NCPU=$(s hw.ncpu)
PHYSMEM=$(s hw.physmem)
JAILED=$(s security.jail.jailed)
JID=$(s security.jail.param.name)
VMGUEST=$(s kern.vm_guest)
LINUXABI=$(s compat.linux.osrelease)

# physmem in GiB to one decimal, without bc/awk (sh does 64-bit arithmetic).
MEMT=$(( PHYSMEM * 10 / 1073741824 ))
MEMGB="$(( MEMT / 10 )).$(( MEMT % 10 ))"

# df -h / without awk: sh word-splitting is enough.
# shellcheck disable=SC2046
set -- $(df -h / | sed -n 2p)
DFS=${1:-?}; DSIZE=${2:-?}; DUSED=${3:-?}; DAVAIL=${4:-?}
case "$DFS" in *containers*) DFS='zfs' ;; esac

case "$JAILED" in
1) JAILTXT="yes  ${D}(a container on FreeBSD *is* a jail)${R}" ;;
*) JAILTXT="no   ${D}(unexpected -- not running under podman?)${R}" ;;
esac

# freebsd-version -k reads /boot/kernel, which no container ships: the kernel
# belongs to the jailmachine guest, not to this image. Say so instead of
# leaking "unable to locate kernel" at the user.
KERNVER=$(freebsd-version -k 2>/dev/null) || KERNVER=''
[ -n "$KERNVER" ] || KERNVER="${OSRELEASE}  ${D}(via kern.osrelease; /boot is outside the container)${R}"

printf '\n'
printf "${C}"
cat <<'BEASTIE'
       ,        ,
      /(        )`
      \ \___   / |
      /- _  `-/  '
     (/\/ \ \   /\
     / /   | `    \
     O O   ) /    |
     `-^--'`<     '
    (_.)  _  )   /
     `.___/`    /
       `-----' /
  <----.     __ / __   \
  <----|====O)))==) \) /====
  <----'    `--' `.__,' \
               |        |
                \       /
           ______( (_  / \______
         ,'  ,-----'   |        \
         `--{__________)        \/
BEASTIE
printf "${R}"

printf "\n${B}┌────────────────────────────────────────────────────────────────────┐${R}\n"
printf "${B}│${R}  ${B}jailmachine demo${R} · ${G}this really is FreeBSD${R}                         ${B}│${R}\n"
printf "${B}└────────────────────────────────────────────────────────────────────┘${R}\n\n"

row() { printf "  ${Y}%-18s${R} %b\n" "$1" "$2"; }

row 'kernel'        "${B}$(uname -s) $(uname -r)${R}  ${D}$(uname -m)${R}"
row 'uname -a'      "${D}$(uname -a)${R}"
row 'userland'      "$(freebsd-version -u 2>/dev/null || echo "$OSRELEASE")"
row 'running kernel' "$KERNVER"
row 'kern.osreldate' "$OSRELDATE  ${D}(__FreeBSD_version)${R}"
row 'ABI'           "${B}${ARCH}${R}  ${D}(hw.machine=${MACHINE})${R}"
row 'jailed?'       "$JAILTXT"
row 'jail id'       "${JID}  ${D}(security.jail.param.name)${R}"
row 'vm guest'      "${VMGUEST}  ${D}(kern.vm_guest -- we are inside the jm VM)${R}"
row 'cpus / memory' "${NCPU} cores / ${MEMGB} GiB  ${D}(what jm gave the guest)${R}"
row 'linux ABI'     "${LINUXABI}  ${D}(compat.linux.osrelease -- the Linuxulator)${R}"
row 'root fs'       "${DFS}  ${D}(a ZFS dataset in the guest -- not your Mac's disk)${R}"
row 'disk'          "${DUSED} used of ${DSIZE}, ${DAVAIL} free"
row 'hostname'      "$(hostname)  ${D}(= the container id)${R}"

printf "\n  ${D}No Linux was harmed in the making of this container. There is no${R}\n"
printf "  ${D}Linux kernel anywhere in this stack -- podman is talking to a${R}\n"
printf "  ${D}FreeBSD 15.1 kernel, and this container is a jail.${R}\n\n"
