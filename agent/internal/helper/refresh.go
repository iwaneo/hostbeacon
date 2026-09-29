package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
)

// DefaultPackageListStamp holds the time of the last successful package list
// refresh. Root writes it; the network part reads it for the Available
// updates group.
const DefaultPackageListStamp = "/var/lib/hostbeacon-helper/package-list-refreshed"

// PackageListMaxAge: the list is refreshed only when it is older (v1 spec
// §4.6).
const PackageListMaxAge = 24 * time.Hour

// RefreshResult says what a package list refresh did.
type RefreshResult string

const (
	RefreshDone         RefreshResult = "refreshed"
	RefreshFresh        RefreshResult = "fresh"         // refreshed less than 24 hours ago
	RefreshBusy         RefreshResult = "busy"          // a package task or the package manager holds its lock
	RefreshOff          RefreshResult = "off"           // turned off in the Host config
	RefreshNotSupported RefreshResult = "not_supported" // no apt or dnf
)

// PackageListRefresh refreshes the package list for the fixed root timer
// (v1 spec §4.6). It is not a helper job: nothing on the network side can
// start it.
type PackageListRefresh struct {
	Manager                string // apt or dnf; "" on distros without full support
	Run                    command.Command
	Stamp                  string
	PackageTaskLock        string
	PackageManagerLocks    []string
	PackageManagerPIDLocks []string
	Now                    func() time.Time
}

// Refresh refreshes the list if it is on, older than 24 hours, and no
// package task or package manager holds its lock. It takes the package-task
// lock for the whole refresh.
func (p PackageListRefresh) Refresh(ctx context.Context, cfg config.Config) (RefreshResult, error) {
	if !cfg.PackageListRefresh {
		return RefreshOff, nil
	}
	var args []string
	switch p.Manager {
	case "apt":
		// Without Error-Mode, apt-get exits 0 when a source cannot be
		// fetched. Older apt ignores the option.
		args = []string{"apt-get", "update", "-q", "-o", "APT::Update::Error-Mode=any"}
	case "dnf":
		args = []string{"dnf", "makecache", "--refresh", "-q"}
	default:
		return RefreshNotSupported, nil
	}
	now := p.Now()
	// A stamp from the future (the clock went back) is not trusted.
	if last, ok := ReadPackageListStamp(p.Stamp); ok && !last.After(now) && now.Sub(last) < PackageListMaxAge {
		return RefreshFresh, nil
	}
	if err := os.MkdirAll(filepath.Dir(p.PackageTaskLock), 0o755); err != nil {
		return "", err
	}
	release, err := lockPackageTask(p.PackageTaskLock)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return RefreshBusy, nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot take the package-task lock: %w", err)
	}
	defer release()
	held, err := anyLockHeld(p.PackageManagerLocks)
	if err != nil {
		return "", fmt.Errorf("cannot check the package manager's locks: %w", err)
	}
	if held || anyPIDLockHeld(p.PackageManagerPIDLocks) {
		return RefreshBusy, nil
	}
	if _, err := p.Run(ctx, args[0], args[1:]...); err != nil {
		return "", fmt.Errorf("%s failed: %w", strings.Join(args[:2], " "), withStderr(err))
	}
	if err := writeStamp(p.Stamp, p.Now()); err != nil {
		return "", err
	}
	return RefreshDone, nil
}

const stampFormat = 1

// stamp holds the time of the last successful package list refresh. Later
// releases may add fields; this one ignores them.
type stamp struct {
	Format      int       `json:"format"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

// writeStamp stores the time of a successful package list refresh.
func writeStamp(path string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(stamp{Format: stampFormat, RefreshedAt: now.UTC().Truncate(time.Second)})
	if err != nil {
		return err
	}
	return statefile.Write(path, append(data, '\n'), 0o644)
}

// DetectPackageManager returns apt, dnf, or "" on distros without full
// support.
func DetectPackageManager(root string) string {
	for _, manager := range [][2]string{{"apt", "apt-get"}, {"dnf", "dnf"}} {
		if _, err := os.Stat(filepath.Join(root, "usr", "bin", manager[1])); err == nil {
			return manager[0]
		}
	}
	return ""
}

// ReadPackageListStamp reads the time of the last successful refresh. ok is
// false when there is none, or it cannot be read.
func ReadPackageListStamp(path string) (last time.Time, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	var s stamp
	if err := json.Unmarshal(data, &s); err != nil || s.Format != stampFormat {
		return time.Time{}, false
	}
	return s.RefreshedAt, true
}

// withStderr adds the last line the program wrote to its standard error.
func withStderr(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		lines := strings.Split(strings.TrimSpace(string(exit.Stderr)), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return fmt.Errorf("%w: %s", err, last)
		}
	}
	return err
}
