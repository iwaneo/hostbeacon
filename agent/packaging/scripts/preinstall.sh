#!/bin/sh
# Runs before the .deb or .rpm unpacks its files.
set -e

# Hostbeacon needs systemd as the running init system (v1 spec §2).
if [ ! -d /run/systemd/system ]; then
	echo "Hostbeacon needs systemd, and systemd is not running on this Host. Nothing was installed." >&2
	exit 1
fi

# The tarball's units in /etc would hide the package's units.
if [ -e /usr/local/bin/hostbeacon ]; then
	cat >&2 <<'MESSAGE'
Hostbeacon is installed from the tarball already. Remove that install first
(this keeps the Agent's identity and Pairings), then install the package again:
  sudo systemctl disable --now hostbeacon.service hostbeacon-helper.service hostbeacon-package-list-refresh.timer
  sudo rm /usr/local/bin/hostbeacon /usr/local/bin/hostbeacon-helper /etc/systemd/system/hostbeacon*.service /etc/systemd/system/hostbeacon*.timer /etc/sysusers.d/hostbeacon.conf /etc/tmpfiles.d/hostbeacon.conf
MESSAGE
	exit 1
fi
