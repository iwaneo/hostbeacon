//go:build hosttest

// This test runs on a real Debian or Fedora test Host, as root:
//
//	GOOS=linux GOARCH=amd64 go test -c -tags hosttest -o helper-host.test ./internal/helper
//	sudo ./helper-host.test -test.run TestHost -test.v
//
// It refreshes the package list and simulates or downloads the Update run.
// On apt, it also installs a small test package from a local repository,
// and removes both at the end.
package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
)

// v1 spec §8 step 4: what is installed is exactly the transaction the checks
// approved. Here the plan changes between the check and the install, as when
// another tool refreshes the package list and a package's newest version
// changes: the run must stop and install nothing.
func TestHostChangedPlanStopsTheInstall(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	env := []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}
	u := &UpdateRun{
		Manager:   DetectPackageManager("/"),
		Run:       command.ExecFor(env, time.Hour),
		Stream:    command.ExecStream(env),
		Root:      "/",
		Downloads: t.TempDir(),
	}
	var manager packageManager
	switch u.Manager {
	case "apt":
		manager = aptRun{u}
	case "dnf":
		manager = dnfRun{u}
	default:
		t.Skip("no apt or dnf")
	}
	ctx := context.Background()
	if err := manager.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Installs) == 0 {
		t.Skip("no Available updates on this Host, so there is no plan to change")
	}
	t.Logf("approved plan: %+v", plan.Installs)
	before := installedPackages(t, u)

	changed := plan
	changed.Installs = slices.Clone(plan.Installs)
	changed.Installs[0].Version += "+changed"
	err = manager.install(ctx, changed, func(float64) {})
	if err == nil {
		t.Fatal("the install went on with a changed plan")
	}
	t.Logf("the run stopped: %v", err)
	if after := installedPackages(t, u); after != before {
		t.Fatal("the installed packages changed")
	}
}

// The same on apt with a real change: another tool refreshes the package
// list between the check and the install, and the approved version is gone
// (as when a security update replaces it).
func TestHostAptListRefreshBetweenCheckAndInstall(t *testing.T) {
	u, manager, run, publish := testRepository(t)
	ctx := context.Background()
	publish("2.0")
	if err := manager.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Installs, planned{Name: testPackage, Arch: "all", Version: "2.0"}) {
		t.Fatalf("approved plan %+v has no %s 2.0", plan.Installs, testPackage)
	}
	before := installedPackages(t, u)

	// Another tool refreshes the list: 2.0 is gone, 3.0 is new.
	publish("3.0")
	run("apt-get", "update", "-q")
	err = manager.install(ctx, plan, func(float64) {})
	if err == nil {
		t.Fatal("the install went on after the package list changed")
	}
	t.Logf("the run stopped: %v", err)
	if after := installedPackages(t, u); after != before {
		t.Fatal("the installed packages changed")
	}
}

// An installed update of an automatically installed package stays
// automatically installed, so autoremove still works.
func TestHostAptInstallKeepsAutomaticPackagesAutomatic(t *testing.T) {
	_, manager, run, publish := testRepository(t)
	ctx := context.Background()
	run("apt-mark", "auto", testPackage)
	publish("2.0")
	if err := manager.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Only the test package: this test must not update the Host.
	test := transaction{Installs: []planned{{Name: testPackage, Arch: "all", Version: "2.0"}}}
	if !slices.Contains(plan.Installs, test.Installs[0]) {
		t.Fatalf("approved plan %+v has no %s 2.0", plan.Installs, testPackage)
	}
	if err := manager.install(ctx, test, func(float64) {}); err != nil {
		t.Fatal(err)
	}
	if version := run("dpkg-query", "-W", "-f", "${Version}", testPackage); version != "2.0" {
		t.Fatalf("installed %q", version)
	}
	if !slices.Contains(strings.Fields(run("apt-mark", "showauto")), testPackage) {
		t.Fatal("the updated package is now marked as manually installed")
	}
}

const testPackage = "hostbeacon-test-dummy"

// testRepository installs version 1.0 of a small test package and makes a
// local repository; publish puts one version of the package in it. Both
// are removed at the end.
func testRepository(t *testing.T) (*UpdateRun, aptRun, func(...string) string, func(version string)) {
	t.Helper()
	if os.Geteuid() != 0 || DetectPackageManager("/") != "apt" {
		t.Skip("needs root and apt")
	}
	env := []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}
	u := &UpdateRun{Manager: "apt", Run: command.ExecFor(env, time.Hour), Stream: command.ExecStream(env), Root: "/"}
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := u.Run(ctx, args[0], args[1:]...)
		if err != nil {
			t.Fatalf("%s: %v", strings.Join(args, " "), withStderr(err))
		}
		return string(out)
	}

	dir, err := os.MkdirTemp("/var/tmp", "hostbeacon-test-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	const name, source = testPackage, "/etc/apt/sources.list.d/hostbeacon-test.list"
	t.Cleanup(func() {
		u.Run(ctx, "dpkg", "--purge", name)
		os.Remove(source)
		os.RemoveAll(dir)
		u.Run(ctx, "apt-get", "update", "-q")
	})
	debs := map[string]string{}
	for _, version := range []string{"1.0", "2.0", "3.0"} {
		root := filepath.Join(dir, "build-"+version)
		os.MkdirAll(filepath.Join(root, "DEBIAN"), 0o755)
		control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: all\nMaintainer: Hostbeacon test <test@invalid>\nDescription: Hostbeacon test package, safe to remove\n", name, version)
		os.WriteFile(filepath.Join(root, "DEBIAN", "control"), []byte(control), 0o644)
		debs[version] = filepath.Join(dir, name+"_"+version+"_all.deb")
		run("dpkg-deb", "--root-owner-group", "--build", root, debs[version])
	}
	run("dpkg", "-i", debs["1.0"])
	repo := filepath.Join(dir, "repo")
	publish := func(version string) {
		os.RemoveAll(repo)
		os.MkdirAll(repo, 0o755)
		data, _ := os.ReadFile(debs[version])
		file := filepath.Base(debs[version])
		os.WriteFile(filepath.Join(repo, file), data, 0o644)
		sum := sha256.Sum256(data)
		packages := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: all\nMaintainer: Hostbeacon test <test@invalid>\nFilename: ./%s\nSize: %d\nSHA256: %s\nDescription: Hostbeacon test package, safe to remove\n\n",
			name, version, file, len(data), hex.EncodeToString(sum[:]))
		os.WriteFile(filepath.Join(repo, "Packages"), []byte(packages), 0o644)
		index := sha256.Sum256([]byte(packages))
		release := fmt.Sprintf("Origin: hostbeacon-test\nLabel: hostbeacon-test\nDate: %s\nSHA256:\n %s %d Packages\n",
			time.Now().UTC().Format(time.RFC1123), hex.EncodeToString(index[:]), len(packages))
		os.WriteFile(filepath.Join(repo, "Release"), []byte(release), 0o644)
	}
	if err := os.WriteFile(source, []byte("deb [trusted=yes] file:"+repo+" ./\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return u, aptRun{u}, run, publish
}

func installedPackages(t *testing.T, u *UpdateRun) string {
	t.Helper()
	args := []string{"dpkg-query", "-W", "-f", "${Package}:${Architecture}=${Version} ${Status}\n"}
	if u.Manager == "dnf" {
		args = []string{"rpm", "-qa", "--queryformat", "%{NAME}.%{ARCH} %{EPOCHNUM}:%{VERSION}-%{RELEASE}\n"}
	}
	out, err := u.Run(context.Background(), args[0], args[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
