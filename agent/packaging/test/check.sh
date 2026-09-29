#!/bin/sh
# Runs inside a test container: installs the Agent one way and checks it.
# Usage: check.sh deb | rpm | tarball | deb-install | rpm-install
# The -install kinds only install; the no-systemd check uses them.
set -eu
kind=$1

fail() {
	echo "FAIL: $*" >&2
	journalctl --no-pager -n 40 -u hostbeacon -u hostbeacon-helper >&2 || true
	exit 1
}

case $(uname -m) in
x86_64) arch=amd64 rpmarch=x86_64 ;;
aarch64) arch=arm64 rpmarch=aarch64 ;;
esac
export DEBIAN_FRONTEND=noninteractive
install_deb() { apt-get install -y "$(ls /dist/hostbeacon_*_"$arch".deb)"; }
# No repository: the package needs nothing from the network.
install_rpm() { dnf install -y --disablerepo='*' "$(ls /dist/hostbeacon-*."$rpmarch".rpm)"; }

case $kind in
deb-install) install_deb; exit ;;
rpm-install) install_rpm; exit ;;
esac

# Wait until systemd has booted, so it does not mount over what the test makes.
for _ in $(seq 60); do
	case $(systemctl is-system-running 2>/dev/null) in
	running | degraded) break ;;
	esac
	sleep 1
done
case $kind in
deb) install_deb ;;
rpm) install_rpm ;;
tarball)
	dir=$(mktemp -d /root/tarball.XXXXXX)
	tar -C "$dir" -xzf "$(ls /dist/hostbeacon_*_linux_"$arch".tar.gz)"
	(cd "$dir"/hostbeacon_* && ./hostbeacon install)
	;;
esac

echo "-- the user, the state directory, and root-owned files"
getent passwd hostbeacon | grep -q nologin || fail "the hostbeacon user has a login shell: $(getent passwd hostbeacon)"
[ "$(stat -c '%a %U' /var/lib/hostbeacon)" = "700 hostbeacon" ] ||
	fail "/var/lib/hostbeacon is $(stat -c '%a %U' /var/lib/hostbeacon), want 700 hostbeacon"
for file in /usr/bin/hostbeacon* /usr/local/bin/hostbeacon* /usr/lib/systemd/system/hostbeacon* \
	/etc/systemd/system/hostbeacon* /etc/hostbeacon /etc/hostbeacon/config.json; do
	[ -e "$file" ] || continue
	[ "$(stat -c %U "$file")" = root ] || fail "$file is not owned by root"
	[ -z "$(find "$file" -maxdepth 0 -perm /022)" ] || fail "$file can be changed by others than root"
done

echo "-- the Agent starts"
for _ in $(seq 30); do
	journalctl --no-pager -u hostbeacon | grep -q 'msg=listening' && break
	sleep 1
done
journalctl --no-pager -u hostbeacon | grep -q 'msg=listening' || fail "the Agent did not start listening"
systemctl is-active --quiet hostbeacon-helper.service || fail "the root helper is not running"
systemctl is-enabled --quiet hostbeacon-package-list-refresh.timer || fail "the package list refresh timer is not enabled"

echo "-- no Action is on"
status=$(hostbeacon status)
echo "$status"
echo "$status" | grep -q '^Agent: *running' || fail "status does not say the Agent runs"
echo "$status" | grep -q '^Actions: *none' || fail "an Action is on after install"

echo "-- the network part does not make its state directory"
systemctl stop hostbeacon.service
mv /var/lib/hostbeacon /var/lib/hostbeacon.saved
out=$(systemd-run --quiet --wait --pipe --property=User=hostbeacon "$(command -v hostbeacon)" serve 2>&1) &&
	fail "serve ran without /var/lib/hostbeacon"
echo "$out" | grep -q 'stat /var/lib/hostbeacon: no such file or directory' || fail "serve said: $out"
mv /var/lib/hostbeacon.saved /var/lib/hostbeacon
systemctl start hostbeacon.service

echo "-- setup, flags-only form"
hostbeacon setup --actions reboot,update_run --vpn-address 100.101.102.103
hostbeacon status | grep -q '^Actions: *Reboot, Update run' || fail "setup did not turn on the Actions"
systemctl is-active --quiet hostbeacon.service || fail "the Agent does not run after setup"

id=$(grep -o '"instance_id": *"[^"]*"' /var/lib/hostbeacon/identity.json)
case $kind in
deb)
	echo "-- remove keeps the instance ID; purge removes it"
	apt-get remove -y hostbeacon
	grep -q "$id" /var/lib/hostbeacon/identity.json || fail "remove lost the instance ID"
	! systemctl is-active --quiet hostbeacon.service || fail "the Agent runs after remove"
	apt-get purge -y hostbeacon
	[ ! -e /var/lib/hostbeacon ] || fail "purge kept /var/lib/hostbeacon"
	[ ! -e /etc/hostbeacon ] || fail "purge kept /etc/hostbeacon"
	;;
rpm)
	echo "-- remove keeps the instance ID"
	dnf remove -y hostbeacon
	grep -q "$id" /var/lib/hostbeacon/identity.json || fail "remove lost the instance ID"
	! systemctl is-active --quiet hostbeacon.service || fail "the Agent runs after remove"
	;;
esac
echo "OK: $kind"
