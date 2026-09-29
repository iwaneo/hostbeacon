#!/bin/sh
# Runs after the .deb or .rpm removed its files. A remove keeps the Agent's
# identity, Pairings, and Host config; a purge (.deb only) removes them, so a
# new install is a new Host (v1 spec §4.3).
set -e
case "$1" in
remove | 0)
	systemctl daemon-reload || true
	;;
purge)
	rm -rf /etc/hostbeacon /var/lib/hostbeacon /var/cache/hostbeacon \
		/var/lib/hostbeacon-helper /var/cache/hostbeacon-helper /var/log/hostbeacon
	;;
esac
