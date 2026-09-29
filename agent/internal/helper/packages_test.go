package helper

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const aptSimulation = `NOTE: This is only a simulation!
Reading package lists...
Inst linux-image-6.12.111+deb13-cloud-amd64 (6.12.111-1 Debian-Security:13/stable-security [amd64])
Inst linux-image-cloud-amd64 [6.12.107-1] (6.12.111-1 Debian-Security:13/stable-security [amd64])
Inst tzdata [2026b-0+deb13u1] (1:2026c-0+deb13u1 Debian:13.7/stable, Debian-Security:13/stable-security [all])
Inst libc6:i386 [2.41-12] (2.41-12+deb13u1 Debian:13.7/stable [i386])
Remv oldpkg [1.0]
Conf linux-image-cloud-amd64 (6.12.111-1 Debian-Security:13/stable-security [amd64])
`

func TestParseAptSimulationReadsInstallsAndRemovals(t *testing.T) {
	plan := parseAptSimulation(aptSimulation)
	want := []planned{
		{Name: "linux-image-6.12.111+deb13-cloud-amd64", Arch: "amd64", Version: "6.12.111-1"},
		{Name: "linux-image-cloud-amd64", Arch: "amd64", Version: "6.12.111-1"},
		{Name: "tzdata", Arch: "all", Version: "1:2026c-0+deb13u1"},
		{Name: "libc6", Arch: "i386", Version: "2.41-12+deb13u1"},
	}
	if !slices.Equal(plan.Installs, want) {
		t.Errorf("installs %+v, want %+v", plan.Installs, want)
	}
	if !slices.Equal(plan.Removes, []string{"oldpkg"}) {
		t.Errorf("removes %q", plan.Removes)
	}
	args := aptInstallArgs(plan.Installs)
	if !slices.Equal(args, []string{
		"linux-image-6.12.111+deb13-cloud-amd64:amd64=6.12.111-1", "linux-image-cloud-amd64:amd64=6.12.111-1",
		"tzdata:all=1:2026c-0+deb13u1", "libc6:i386=2.41-12+deb13u1",
	}) {
		t.Errorf("install args %q", args)
	}
}

// writeRelease writes one package index's Release file in lists.
func writeRelease(t *testing.T, lists, name, origin, codename string) {
	t.Helper()
	data := "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nOrigin: " + origin + "\nLabel: " + origin + "\nSuite: stable\nCodename: " + codename + "\nSHA256:\n 0123 100 main/binary-amd64/Packages\n"
	if err := os.WriteFile(filepath.Join(lists, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAptReleaseCheck(t *testing.T) {
	for _, test := range []struct {
		name     string
		codename string
		indexes  [][2]string // origin, codename
		changes  bool
	}{
		{"same release", "trixie", [][2]string{{"Debian", "trixie"}, {"Debian", "trixie-updates"}, {"Debian", "trixie-security"}, {"Debian Backports", "trixie-backports"}}, false},
		{"ubuntu", "noble", [][2]string{{"Ubuntu", "noble"}, {"Ubuntu", "noble-updates"}, {"Ubuntu", "noble-security"}}, false},
		{"other origins are not checked", "trixie", [][2]string{{"Debian", "trixie"}, {"Proxmox", "bookworm"}, {"Docker", "whatever"}}, false},
		{"stable alias moved to the next release", "trixie", [][2]string{{"Debian", "forky"}}, true},
		{"next Ubuntu release", "noble", [][2]string{{"Ubuntu", "noble"}, {"Ubuntu", "plucky-updates"}}, true},
		{"proposed updates are not allowed", "trixie", [][2]string{{"Debian", "trixie-proposed-updates"}}, true},
		{"no codename on the Host", "", [][2]string{{"Debian", "sid"}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lists := t.TempDir()
			for i, index := range test.indexes {
				writeRelease(t, lists, "source"+string(rune('a'+i))+"_InRelease", index[0], index[1])
			}
			// Signatures and package lists are not Release files.
			os.WriteFile(filepath.Join(lists, "source_Release.gpg"), []byte("Origin: Debian\nCodename: forky\n"), 0o644)
			changes, err := aptReleaseChanges(lists, test.codename)
			if err != nil || changes != test.changes {
				t.Fatalf("changes %v, error %v, want %v", changes, err, test.changes)
			}
		})
	}
}

func TestAptReleaseCheckWithoutReleaseFilesFailsClosed(t *testing.T) {
	if changes, err := aptReleaseChanges(t.TempDir(), "trixie"); err == nil && !changes {
		t.Fatal("no package index was read, but the check passed")
	}
}

func TestParseRPMHeaders(t *testing.T) {
	got := parseRPMHeaders("python3-urllib3 noarch 0:2.8.0-1.fc44\nkernel-core x86_64 0:6.17.1-300.fc44\n\n")
	want := []planned{
		{Name: "python3-urllib3", Arch: "noarch", Version: "0:2.8.0-1.fc44"},
		{Name: "kernel-core", Arch: "x86_64", Version: "0:6.17.1-300.fc44"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestDnfReleaseCheck(t *testing.T) {
	owners := []planned{{Name: "fedora-release-identity-cloud", Version: "44"}}
	for _, test := range []struct {
		name     string
		owners   []planned
		installs []planned
		changes  bool
	}{
		{"owner not in the run", owners, []planned{{Name: "kernel", Version: "0:6.17-1.fc44"}}, false},
		{"owner updated in the same release", owners, []planned{{Name: "fedora-release-identity-cloud", Version: "0:44-27"}}, false},
		{"owner moves to the next release", owners, []planned{{Name: "fedora-release-identity-cloud", Version: "0:45-1"}}, true},
		{"minor release is not a release change", []planned{{Name: "rocky-release", Version: "9.4"}}, []planned{{Name: "rocky-release", Version: "0:9.5-1.el9"}}, false},
		{"major release is", []planned{{Name: "rocky-release", Version: "9.4"}}, []planned{{Name: "rocky-release", Version: "0:10.0-1.el10"}}, true},
		{"no owner found", nil, []planned{{Name: "kernel", Version: "0:6.17-1.fc44"}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := dnfReleaseChanges(test.owners, test.installs); got != test.changes {
				t.Fatalf("changes %v, want %v", got, test.changes)
			}
		})
	}
}

func TestCountAptUpgradable(t *testing.T) {
	out := "Listing...\nlinux-image-cloud-amd64/stable-security 6.12.111-1 amd64 [upgradable from: 6.12.107-1]\ntzdata/stable 2026c all [upgradable from: 2026b]\n"
	if got := countAptUpgradable(out); got != 2 {
		t.Fatalf("got %d", got)
	}
}

func TestReadOSRelease(t *testing.T) {
	got := parseOSRelease("ID=debian\nVERSION_CODENAME=trixie\nNAME=\"Debian GNU/Linux\"\n# comment\nPRETTY_NAME='x'\n")
	if got["VERSION_CODENAME"] != "trixie" || got["NAME"] != "Debian GNU/Linux" || got["PRETTY_NAME"] != "x" {
		t.Fatalf("got %v", got)
	}
}
