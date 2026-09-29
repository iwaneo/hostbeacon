// Package install holds the files the .deb, the .rpm, and the tarball install
// (systemd units, sysusers.d, tmpfiles.d), and installs the tarball.
package install

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
)

// files are the install files. The units run the programs from /usr/bin,
// where the packages put them.
//
//go:embed files
var files embed.FS

// Units are the units a new install enables and starts, in this order.
var Units = []string{"hostbeacon-helper.service", "hostbeacon.service", "hostbeacon-package-list-refresh.timer"}

// NoSystemd is the message when systemd is not running.
const NoSystemd = "Hostbeacon needs systemd, and systemd is not running on this Host. Nothing was installed."

// Tarball installs the Agent from an unpacked tarball, for distros without
// a .deb or .rpm (v1 spec §4.3). Everything goes under /usr/local and /etc,
// owned by root and writable only by root.
type Tarball struct {
	// Root is the Host's root, "/" outside tests.
	Root string
	// Source is the directory holding hostbeacon and hostbeacon-helper.
	Source string
	Run    command.Command
	Out    io.Writer
}

// Install installs or reinstalls the Agent and starts it. It enables no
// Action and opens no firewall port.
func (t Tarball) Install(ctx context.Context) error {
	if info, err := os.Stat(filepath.Join(t.Root, "run/systemd/system")); err != nil || !info.IsDir() {
		return errors.New(NoSystemd)
	}
	if _, err := os.Stat(filepath.Join(t.Root, "usr/bin/hostbeacon")); err == nil {
		return errors.New("Hostbeacon is installed from a package (.deb or .rpm) already. Update it with `sudo hostbeacon update`, or remove the package first")
	}
	bin := filepath.Join(t.Root, "usr/local/bin")
	for _, name := range []string{"hostbeacon", "hostbeacon-helper"} {
		if err := copyProgram(filepath.Join(t.Source, name), filepath.Join(bin, name)); err != nil {
			return err
		}
	}
	err := eachFile(func(name string, data []byte) error {
		dir := map[string]string{"systemd": "etc/systemd/system", "sysusers.d": "etc/sysusers.d", "tmpfiles.d": "etc/tmpfiles.d"}[path.Dir(name)]
		text := strings.ReplaceAll(string(data), "/usr/bin/hostbeacon", "/usr/local/bin/hostbeacon")
		return writeFile(filepath.Join(t.Root, dir, path.Base(name)), []byte(text), 0o644)
	})
	if err != nil {
		return err
	}
	created, err := config.WriteDefault(filepath.Join(t.Root, config.DefaultPath))
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"systemd-sysusers", "/etc/sysusers.d/hostbeacon.conf"},
		{"systemd-tmpfiles", "--create", "/etc/tmpfiles.d/hostbeacon.conf"},
		{"systemctl", "daemon-reload"},
		append([]string{"systemctl", "enable"}, Units...),
		// Restart, so a reinstall runs the new programs.
		append([]string{"systemctl", "restart"}, Units...),
	} {
		if _, err := t.Run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Fprintln(t.Out, "Hostbeacon is installed in /usr/local/bin and running. No Action is turned on.")
	if created {
		fmt.Fprintln(t.Out, "Next, turn on the Actions you want and pair Home Assistant:\n  sudo hostbeacon setup")
	}
	return nil
}

// eachFile calls fn with each install file's name under files/ and its data.
func eachFile(fn func(name string, data []byte) error) error {
	dirs, err := files.ReadDir("files")
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		entries, err := files.ReadDir("files/" + dir.Name())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := dir.Name() + "/" + entry.Name()
			data, err := files.ReadFile("files/" + name)
			if err != nil {
				return err
			}
			if err := fn(name, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyProgram copies a program in one step, so a running copy is never half
// replaced. Copying a file onto itself does nothing.
func copyProgram(from, to string) error {
	source, err := os.Stat(from)
	if err != nil {
		return err
	}
	if target, err := os.Stat(to); err == nil && os.SameFile(source, target) {
		return nil
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return writeFile(to, data, 0o755)
}

func writeFile(name string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	return statefile.Write(name, data, perm)
}
