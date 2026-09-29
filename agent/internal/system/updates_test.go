package system

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const aptList = "apt list --upgradable -o APT::Cmd::Disable-Script-Warning=true"
const dnfUpgrades = "dnf --cacheonly --quiet repoquery --upgrades --latest-limit=1 --queryformat %{name} %{arch} %{evr}\\n"
const rpmInstalled = "rpm -qa --queryformat %{NAME} %{ARCH} %{EVR}\\n"

func TestAptAvailableUpdates(t *testing.T) {
	run := fakeCommands{aptList: {out: `Listing...
tzdata/stable 2026c-0+deb13u1 all [upgradable from: 2020a-0]
base-files/stable,stable-security 13.8+deb13u7 arm64 [upgradable from: 13.0]
libc6/stable 2.41-12 arm64 [upgradable from: 2.41-10]
libc6/stable 2.41-12 i386 [upgradable from: 2.41-10]
`}}.run
	updates, err := ReadAvailableUpdates(context.Background(), run, "apt")
	if err != nil {
		t.Fatal(err)
	}
	if updates.Count == nil || *updates.Count != 4 || len(updates.Packages) != 4 {
		t.Fatalf("updates = %+v, want 4", updates)
	}
	first := updates.Packages[0]
	if first.Name != "base-files" || first.InstalledVersion != "13.0" || first.NewVersion != "13.8+deb13u7" {
		t.Errorf("first package = %+v, want base-files 13.0 to 13.8+deb13u7 (sorted by name)", first)
	}
	if updates.Fingerprint == nil || len(*updates.Fingerprint) != 64 || updates.LastRefresh != nil {
		t.Errorf("fingerprint %v, last refresh %v", updates.Fingerprint, updates.LastRefresh)
	}
}

func TestDnfAvailableUpdates(t *testing.T) {
	// dnf4 prints a blank line after each package; dnf5 does not.
	run := fakeCommands{
		dnfUpgrades:  {out: "curl aarch64 8.11.1-9.fc42\n\nkernel-core aarch64 6.16.2-200.fc42\n\nnew-dep noarch 1.0-1.fc42\n"},
		rpmInstalled: {out: "curl aarch64 8.11.1-8.fc42\nkernel-core aarch64 6.15.4-200.fc42\nkernel-core aarch64 6.16.1-200.fc42\nglibc aarch64 2.41-16.fc42\n"},
	}.run
	updates, err := ReadAvailableUpdates(context.Background(), run, "dnf")
	if err != nil {
		t.Fatal(err)
	}
	if *updates.Count != 2 {
		t.Fatalf("count = %d, want 2 (a package that is not installed is not an update)", *updates.Count)
	}
	if kernel := updates.Packages[1]; kernel.Name != "kernel-core" || kernel.InstalledVersion != "6.16.1-200.fc42" {
		t.Errorf("kernel = %+v, want the newest installed version", kernel)
	}
}

func TestAvailableUpdatesCapAndFingerprint(t *testing.T) {
	var lines strings.Builder
	for i := range 150 {
		fmt.Fprintf(&lines, "pkg%03d/stable 2 all [upgradable from: 1]\n", i)
	}
	run := fakeCommands{aptList: {out: lines.String()}}.run
	updates, _ := ReadAvailableUpdates(context.Background(), run, "apt")
	if *updates.Count != 150 || len(updates.Packages) != 100 {
		t.Errorf("count %d with %d names, want 150 with 100", *updates.Count, len(updates.Packages))
	}

	// The fingerprint covers the full set, not only the capped list.
	changed := strings.Replace(lines.String(), "pkg149/stable 2", "pkg149/stable 3", 1)
	again, _ := ReadAvailableUpdates(context.Background(), fakeCommands{aptList: {out: changed}}.run, "apt")
	if *again.Fingerprint == *updates.Fingerprint {
		t.Error("the fingerprint did not change when a package past the cap changed")
	}
	same, _ := ReadAvailableUpdates(context.Background(), run, "apt")
	if *same.Fingerprint != *updates.Fingerprint {
		t.Error("the same set gave a different fingerprint")
	}
}

func TestNoAvailableUpdates(t *testing.T) {
	run := fakeCommands{aptList: {out: "Listing...\n"}}.run
	updates, err := ReadAvailableUpdates(context.Background(), run, "apt")
	if err != nil || *updates.Count != 0 || updates.Packages == nil || updates.Fingerprint == nil {
		t.Errorf("updates = %+v, %v; want count 0, an empty list, and a fingerprint", updates, err)
	}
}

func TestAvailableUpdatesThatCannotBeRead(t *testing.T) {
	run := fakeCommands{dnfUpgrades: {err: errors.New("Cache-only enabled but no cache")}}.run
	if _, err := ReadAvailableUpdates(context.Background(), run, "dnf"); err == nil {
		t.Error("want an error")
	}
}
