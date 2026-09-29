//go:build hosttest

// This test runs on a real Debian or Fedora test Host, as root:
//
//	GOOS=linux GOARCH=amd64 go test -c -tags hosttest -o helper-host.test ./internal/helper
//	sudo ./helper-host.test -test.run TestHost -test.v
//
// It refreshes the package list and simulates or downloads the Update run,
// but installs nothing.
package helper

import (
	"context"
	"os"
	"slices"
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
