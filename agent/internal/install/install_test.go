package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
)

// host is a Host's root with systemd running, and a tarball beside it.
func host(t *testing.T) (root, source string) {
	t.Helper()
	root, source = t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hostbeacon", "hostbeacon-helper"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("binary "+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, source
}

type recorder struct{ commands []string }

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, strings.Join(append([]string{name}, args...), " "))
	return nil, nil
}

func TestTarballInstallsBinariesUnitsAndConfig(t *testing.T) {
	root, source := host(t)
	var r recorder
	var out bytes.Buffer
	if err := (Tarball{Root: root, Source: source, Version: "1.0.0", Run: r.run, Out: &out}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hostbeacon", "hostbeacon-helper"} {
		info, err := os.Stat(filepath.Join(root, "usr/local/lib/hostbeacon/1.0.0", name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %o, want 755", name, info.Mode().Perm())
		}
		wantProgram(t, root, name, "binary "+name)
	}
	units, err := os.ReadDir(filepath.Join(root, "etc/systemd/system"))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 6 {
		t.Errorf("%d units, want 6", len(units))
	}
	for _, unit := range units {
		path := filepath.Join(root, "etc/systemd/system", unit.Name())
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "/usr/bin/") {
			t.Errorf("%s runs a program from /usr/bin, want /usr/local/bin:\n%s", unit.Name(), data)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %o, want 644", unit.Name(), info.Mode().Perm())
		}
	}
	service, _ := os.ReadFile(filepath.Join(root, "etc/systemd/system/hostbeacon.service"))
	if !strings.Contains(string(service), "ExecStart=/usr/local/bin/hostbeacon serve") {
		t.Errorf("hostbeacon.service does not run /usr/local/bin/hostbeacon:\n%s", service)
	}
	for _, name := range []string{"etc/sysusers.d/hostbeacon.conf", "etc/tmpfiles.d/hostbeacon.conf"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Error(err)
		}
	}
	c, err := config.Load(filepath.Join(root, "etc/hostbeacon/config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EnabledActions) != 0 {
		t.Errorf("enabled actions = %v, want none after install", c.EnabledActions)
	}
	want := []string{
		"systemd-sysusers /etc/sysusers.d/hostbeacon.conf",
		"systemd-tmpfiles --create /etc/tmpfiles.d/hostbeacon.conf",
		"systemctl daemon-reload",
		"systemctl enable hostbeacon-helper.service hostbeacon.service hostbeacon-package-list-refresh.timer",
		"systemctl restart hostbeacon-helper.service hostbeacon.service hostbeacon-package-list-refresh.timer",
	}
	if !slices.Equal(r.commands, want) {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(r.commands, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "sudo hostbeacon setup") {
		t.Errorf("the output does not say what to run next:\n%s", out.String())
	}
}

func TestTarballKeepsTheOwnerConfig(t *testing.T) {
	root, source := host(t)
	path := filepath.Join(root, "etc/hostbeacon/config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"format": 1, "enabled_actions": ["reboot"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var r recorder
	var out bytes.Buffer
	if err := (Tarball{Root: root, Source: source, Version: "1.0.0", Run: r.run, Out: &out}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := config.Load(path); len(c.EnabledActions) != 1 {
		t.Error("a new install replaced the owner's config")
	}
	if strings.Contains(out.String(), "No Action") {
		t.Errorf("the output says no Action is on, but Reboot is:\n%s", out.String())
	}
}

func TestTarballStopsWithoutSystemd(t *testing.T) {
	root, source := host(t)
	if err := os.RemoveAll(filepath.Join(root, "run/systemd")); err != nil {
		t.Fatal(err)
	}
	var r recorder
	err := (Tarball{Root: root, Source: source, Run: r.run, Out: &bytes.Buffer{}}).Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Fatalf("err = %v, want one that names systemd", err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr")); err == nil || len(r.commands) > 0 {
		t.Error("files were written or commands run without systemd")
	}
}

func TestTarballStopsWhenAPackageIsInstalled(t *testing.T) {
	root, source := host(t)
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/hostbeacon"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	var r recorder
	err := (Tarball{Root: root, Source: source, Run: r.run, Out: &bytes.Buffer{}}).Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "package") {
		t.Fatalf("err = %v, want one that says a package is installed", err)
	}
	if len(r.commands) > 0 {
		t.Error("commands ran")
	}
}

// wantProgram checks that /usr/local/bin/<name> runs a program with content.
func wantProgram(t *testing.T, root, name, content string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(root, "usr/local/bin", name)); err != nil || string(data) != content {
		t.Errorf("/usr/local/bin/%s = %q, %v; want %q", name, data, err, content)
	}
}

func TestTarballInstallFromTheInstalledCopy(t *testing.T) {
	// `sudo hostbeacon install` run again from the installed version keeps working.
	root, source := host(t)
	if err := (Tarball{Root: root, Source: source, Version: "1.0.0", Run: (&recorder{}).run, Out: &bytes.Buffer{}}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(root, "usr/local/lib/hostbeacon/1.0.0")
	if err := (Tarball{Root: root, Source: installed, Version: "1.0.0", Run: (&recorder{}).run, Out: &bytes.Buffer{}}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantProgram(t, root, "hostbeacon", "binary hostbeacon")
}

func TestTarballInstallOfAnotherVersionSwitchesToItAndKeepsTheOld(t *testing.T) {
	// The Agent update installs a new version this way, and a rollback runs
	// the old version's install again.
	root, source := host(t)
	install := func(source, version string) {
		t.Helper()
		if err := (Tarball{Root: root, Source: source, Version: version, Run: (&recorder{}).run, Out: &bytes.Buffer{}}).Install(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	install(source, "1.0.0")
	newer := t.TempDir()
	for _, name := range []string{"hostbeacon", "hostbeacon-helper"} {
		os.WriteFile(filepath.Join(newer, name), []byte("new "+name), 0o755)
	}
	install(newer, "1.1.0")
	wantProgram(t, root, "hostbeacon", "new hostbeacon")
	wantProgram(t, root, "hostbeacon-helper", "new hostbeacon-helper")

	install(filepath.Join(root, "usr/local/lib/hostbeacon/1.0.0"), "1.0.0")
	wantProgram(t, root, "hostbeacon", "binary hostbeacon")
}

func TestTarballReplacesAnInstallWithoutVersions(t *testing.T) {
	root, source := host(t)
	bin := filepath.Join(root, "usr/local/bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "hostbeacon"), []byte("old"), 0o755)
	if err := (Tarball{Root: root, Source: source, Version: "1.0.0", Run: (&recorder{}).run, Out: &bytes.Buffer{}}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantProgram(t, root, "hostbeacon", "binary hostbeacon")
}

func TestPackageUnitsRunTheProgramsFromUsrBin(t *testing.T) {
	// The .deb and .rpm install these files as they are.
	units, err := files.ReadDir("files/systemd")
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		data, _ := files.ReadFile("files/systemd/" + unit.Name())
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Exec") && !strings.Contains(line, "=/usr/bin/hostbeacon") {
				t.Errorf("%s: %s", unit.Name(), line)
			}
		}
	}
}
