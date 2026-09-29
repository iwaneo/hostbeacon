#!/bin/sh
# Installs the Agent in containers of one release that run systemd, and
# checks the result: the .deb or .rpm, then the tarball. Also checks that the
# package refuses to install without systemd. Needs Docker.
# Usage: packaging/test/install-test.sh <image> <directory with the packages>
set -eu
image=$1
dist=$(cd "$2" && pwd)
here=$(cd "$(dirname "$0")" && pwd)
tag=hostbeacon-test-$(printf '%s' "$image" | tr -c 'a-z0-9' '-')

# The release with systemd added. This is the only step that uses the
# network; the installs run offline.
docker build --quiet --tag "$tag" - <<DOCKERFILE
FROM $image
RUN if command -v apt-get >/dev/null; then \
      apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd dbus && rm -rf /var/lib/apt/lists/*; \
    elif command -v dnf >/dev/null; then dnf install -y systemd && dnf clean all; \
    else zypper --non-interactive install systemd; fi
DOCKERFILE

if docker run --rm "$tag" sh -c 'command -v apt-get' >/dev/null; then
	package=deb
elif docker run --rm "$tag" sh -c 'command -v dnf' >/dev/null; then
	package=rpm
else
	package=""
fi

# Runs check.sh <kind> in a new container with systemd as PID 1.
check() {
	name=$tag-$1
	docker rm --force "$name" >/dev/null 2>&1 || true
	docker run --detach --name "$name" --privileged --cgroupns=private --network=none \
		--volume "$dist:/dist:ro" --volume "$here:/test:ro" "$tag" /usr/lib/systemd/systemd >/dev/null
	status=0
	docker exec "$name" sh /test/check.sh "$1" || status=$?
	docker rm --force "$name" >/dev/null
	return $status
}

if [ -n "$package" ]; then
	echo "== $image: no systemd running"
	out=$(docker run --rm --network=none --volume "$dist:/dist:ro" --volume "$here:/test:ro" "$tag" \
		sh /test/check.sh "$package-install" 2>&1) && {
		echo "FAIL: the $package installed without systemd running" >&2
		exit 1
	}
	case $out in
	*"Hostbeacon needs systemd"*) echo "refused, as it should" ;;
	*)
		echo "$out" >&2
		echo "FAIL: no clear message without systemd" >&2
		exit 1
		;;
	esac
	echo "== $image: $package"
	check "$package"
fi
echo "== $image: tarball"
check tarball
echo "== $image: OK"
