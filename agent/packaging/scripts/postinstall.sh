#!/bin/sh
# Runs after the .deb or .rpm installed its files. The .deb passes
# "configure"; the .rpm passes 1 on install and 2 on upgrade.
set -e
case "$1" in
configure | 1 | 2) ;;
*) exit 0 ;;
esac

# The hostbeacon user, then its state directory /var/lib/hostbeacon (0700).
systemd-sysusers hostbeacon.conf
systemd-tmpfiles --create hostbeacon.conf

# The Host config, owned by root, with every Action off until the owner runs
# `hostbeacon setup`. An existing config is kept.
new=no
if [ ! -e /etc/hostbeacon/config.json ]; then
	[ -d /etc/hostbeacon ] || mkdir -m 0755 /etc/hostbeacon
	printf '{\n  "format": 1\n}\n' >/etc/hostbeacon/config.json
	chmod 0644 /etc/hostbeacon/config.json
	new=yes
fi

# The owner turns the list refresh off in the Host config, not with systemctl,
# so enabling the units again on upgrade keeps the owner's choices.
units="hostbeacon-helper.service hostbeacon.service hostbeacon-package-list-refresh.timer"
systemctl daemon-reload
# shellcheck disable=SC2086
systemctl enable $units
# Restart, so an upgrade runs the new programs.
# shellcheck disable=SC2086
if ! systemctl restart $units; then
	echo "Hostbeacon is installed, but did not start. See: sudo systemctl status hostbeacon hostbeacon-helper" >&2
	exit 0
fi
if [ "$new" = yes ]; then
	echo "Hostbeacon is running. No Action is turned on."
	echo "Next, turn on the Actions you want and pair Home Assistant: sudo hostbeacon setup"
fi
