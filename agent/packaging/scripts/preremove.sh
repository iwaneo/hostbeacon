#!/bin/sh
# Runs before the .deb or .rpm removes its files. The .deb passes "remove"
# or "upgrade"; the .rpm passes 0 on remove and 1 on upgrade.
set -e
case "$1" in
remove | 0)
	systemctl disable --now hostbeacon.service hostbeacon-helper.service hostbeacon-package-list-refresh.timer || true
	;;
esac
