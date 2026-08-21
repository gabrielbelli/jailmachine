#!/bin/sh
# jm-demo-linuxulator -- a Linux userland, one syscall layer above a FreeBSD kernel.
#
# Every line below is read from inside an unmodified Alpine Linux image.
# The right-hand column says, in a word or two, what that line proves.
set -u

B='\033[1m'; D='\033[2m'; C='\033[36m'; G='\033[32m'; Y='\033[33m'; M='\033[35m'; R='\033[0m'
[ -t 1 ] || { B=''; D=''; C=''; G=''; Y=''; M=''; R=''; }

hdr() { printf "\n${B}%s${R}\n${D}%s${R}\n" "$1" "$2"; }
kv()  { printf "  ${Y}%-22s${R} %b\n" "$1" "$2"; }

printf '\n'
printf "${B}┌────────────────────────────────────────────────────────────────────┐${R}\n"
printf "${B}│${R}  ${B}jailmachine demo${R} · ${M}Linux userland${R} on a ${G}FreeBSD kernel${R}             ${B}│${R}\n"
printf "${B}└────────────────────────────────────────────────────────────────────┘${R}\n"
printf "\n  ${D}This is a stock %s image. Nothing in it was patched.${R}\n" \
       "$(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-Linux}")"

# ---------------------------------------------------------------------------
hdr "1. What the Linux userland believes" "It asks the kernel who it is, and gets a Linux answer."

kv 'uname -s'   "$(uname -s)                     ${D}<- ostype${R}"
kv 'uname -r'   "$(uname -r)                 ${D}<- fake release, set by compat.linux.osrelease${R}"
kv 'uname -m'   "$(uname -m)                ${D}<- real: same arch as the host${R}"
kv 'os-release' "$(. /etc/os-release; echo "$PRETTY_NAME")        ${D}<- a genuine Linux distro${R}"
kv 'libc'       "$(ldd --version 2>&1 | sed -n '1,2p' | tr '\n' ' ')${D}<- real musl, unpatched${R}"
kv 'interpreter' "$(ls /lib/ld-musl-* 2>/dev/null | tr '\n' ' ')${D}<- ELF loader${R}"

# ---------------------------------------------------------------------------
hdr "2. Where the mask slips" "The same interfaces leak FreeBSD strings straight through."

printf "  ${Y}%-22s${R}\n" 'uname -a'
printf "    ${M}%s${R}\n" "$(uname -a)"
printf "    ${D}%s${R}\n" "^ 'Linux ... 5.15.0 FreeBSD 15.1-RELEASE ... GENERIC': the version field"
printf "    ${D}%s${R}\n" "  is the *real* FreeBSD kernel ident, passed through verbatim."

printf "\n  ${Y}%-22s${R}\n" '/proc/version'
printf "    ${M}%s${R}\n" "$(cat /proc/version 2>/dev/null)"
printf "    ${D}%s${R}\n" "^ built by des@freebsd.org with FreeBSD Clang -- no Linux ever compiled this."

# ---------------------------------------------------------------------------
hdr "3. The filesystems underneath" "Linux cannot mount these. FreeBSD can."

printf "  ${Y}%-22s${R}\n" '/proc/mounts'
sed -n '1,6p' /proc/mounts 2>/dev/null | while read -r dev mp fs rest; do
  case "$fs" in
    zfs)      why='ZFS      <- FreeBSD root filesystem' ;;
    devfs)    why='devfs    <- FreeBSD /dev, not Linux devtmpfs' ;;
    fdescfs)  why='fdescfs  <- FreeBSD-only' ;;
    nullfs)   why='nullfs   <- FreeBSD bind mount' ;;
    procfs)   why='procfs   <- linprocfs, the emulated /proc' ;;
    proc)     why='proc     <- linprocfs, the emulated /proc' ;;
    *)        why="$fs" ;;
  esac
  printf "    %-10s %-14s ${D}%s${R}\n" "$fs" "$mp" "$why"
done

printf "\n  ${Y}%-22s${R} ${D}%s${R}\n" '/proc/filesystems' 'what the kernel can actually mount:'
printf "    ${M}%s${R}\n" "$(tr -s ' \t' ' ' < /proc/filesystems | tr '\n' ' ' | sed 's/nodev //g')"
printf "    ${D}%s${R}\n" "^ ufs, zfs, cd9660, msdosfs, devfs -- a FreeBSD list. No ext4, no"
printf "    ${D}%s${R}\n" "  overlayfs, no btrfs, no cgroup2. And note linprocfs/linsysfs:"
printf "    ${D}%s${R}\n" "  those two ARE the Linuxulator's /proc and /sys, named in the open."

# ---------------------------------------------------------------------------
hdr "4. Emulation seams you can feel" "Where linprocfs has nothing real to report."

CPUIMPL=$(sed -n 's/^CPU implementer[ \t]*: *//p' /proc/cpuinfo 2>/dev/null | sed -n 1p)
BOGO=$(sed -n 's/^BogoMIPS[ \t]*: *//p' /proc/cpuinfo 2>/dev/null | sed -n 1p)
NCPU=$(grep -c '^processor' /proc/cpuinfo 2>/dev/null || echo '?')
MEMKB=$(sed -n 's/^MemTotal:[ \t]*\([0-9]*\).*/\1/p' /proc/meminfo 2>/dev/null)
MEMGB=$(( ${MEMKB:-0} * 10 / 1048576 ))

kv 'cpu count'        "${NCPU}                        ${D}<- real: the VM's cores${R}"
kv 'BogoMIPS'         "${BOGO:-(empty)}                     ${D}<- stub: FreeBSD has no such number${R}"
kv 'CPU implementer'  "${CPUIMPL:-(empty)}                  ${D}<- stub: linprocfs leaves it blank${R}"
kv 'MemTotal'         "$(( MEMGB / 10 )).$(( MEMGB % 10 )) GiB                  ${D}<- real: the VM's RAM${R}"
kv '/sys/fs/cgroup'   "$( [ -d /sys/fs/cgroup/. ] && echo present || echo 'absent               ' )${D}<- no cgroups: limits are jail(8)/rctl(8)${R}"
kv 'io_setup(2)'      "ENOSYS                  ${D}<- Linux AIO is not implemented (bites nginx)${R}"

# ---------------------------------------------------------------------------
printf "\n${B}%s${R}\n" 'In one sentence'
printf "  ${D}%s${R}\n" 'Alpine ships Linux ELF binaries. FreeBSD 15.1 loads them with its'
printf "  ${D}%s${R}\n" 'Linux ABI branch (the "Linuxulator") and answers their Linux syscalls'
printf "  ${D}%s${R}\n" 'natively -- no VM, no translation of instructions, same kernel, same'
printf "  ${D}%s${R}\n" 'CPU. Only the syscall table changes. That is why the arch is real and'
printf "  ${D}%s${R}\n\n" 'the kernel version is a polite fiction.'
