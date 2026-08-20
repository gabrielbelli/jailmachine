#!/bin/sh
# first-boot provisioning, run by nuageinit as root
# Expects JM_SSH_PUBKEY and JM_HOSTNAME in the environment (the seed builder
# prepends "JM_SSH_PUBKEY='...'" / "JM_HOSTNAME='...'" lines to this file).
exec > /var/log/jm-provision.log 2>&1
set -x
: "${JM_HOSTNAME:=jailmachine}"
[ -n "${JM_SSH_PUBKEY:-}" ] || { echo "JM_SSH_PUBKEY not set" >&2; exit 1; }
sysrc hostname="$JM_HOSTNAME"; hostname "$JM_HOSTNAME"
mkdir -p /root/.ssh; chmod 700 /root/.ssh
echo "$JM_SSH_PUBKEY" > /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys
sysrc sshd_enable=YES
sed -i '' -e 's/^#\{0,1\}PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
service sshd restart
# nuageinit runs before DHCP has finished: wait for DNS
i=0; until host -W 2 pkg.freebsd.org >/dev/null 2>&1 || [ $i -ge 60 ]; do sleep 2; i=$((i+1)); done
# packages
env ASSUME_ALWAYS_YES=yes pkg bootstrap -f
pkg install -y podman-suite bastille
# zfs / storage for containers
sysrc zfs_enable=YES
zfs create -o mountpoint=/var/db/containers zroot/containers || true
# linuxulator
sysrc linux_enable=YES; service linux start
# pf (nat for containers), per podman pkg message
ext_if=$(route -n get default | awk '/interface:/{print $2}')
sed -E "s/^(v[46]egress_if) = \"[^\"]*\"/\1 = \"$ext_if\"/" /usr/local/etc/containers/pf.conf.sample > /etc/pf.conf
sysrc pf_enable=YES; service pf start
# podman REST API on a unix socket (the stock 'podman' rc script only restarts containers)
cat > /usr/local/etc/rc.d/podman_service <<'RC'
#!/bin/sh
# PROVIDE: podman_service
# REQUIRE: LOGIN pf
# KEYWORD: shutdown
. /etc/rc.subr
name=podman_service
rcvar=podman_service_enable
: ${podman_service_enable:=NO}
: ${podman_service_socket:="unix:///var/run/podman/podman.sock"}
pidfile=/var/run/podman_service.pid
command=/usr/sbin/daemon
command_args="-f -P ${pidfile} -o /var/log/podman_service.log /usr/local/bin/podman system service --time=0 ${podman_service_socket}"
start_precmd="mkdir -p /var/run/podman"
load_rc_config $name
run_rc_command "$1"
RC
chmod +x /usr/local/etc/rc.d/podman_service
sysrc podman_service_enable=YES; service podman_service start
# bastille
sysrc bastille_enable=YES
touch /var/db/jm-provisioned
