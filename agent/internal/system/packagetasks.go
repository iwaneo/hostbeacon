package system

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// PackageTaskUnits are the systemd units of package tasks. While one runs,
// the flags group says package_task_running (v1 spec §4.6, §6.3).
var PackageTaskUnits = []string{"hostbeacon-package-list-refresh.service", helper.UpdateRunUnit}

// PackageTasks says whether a package task runs, and signals when one starts
// or ends.
type PackageTasks interface {
	PackageTaskRunning(ctx context.Context) (bool, error)
	PackageTaskChanges() <-chan struct{}
}

// PackageTaskRunning says whether a package task unit is running. Reading
// needs no root.
func (s *Systemd) PackageTaskRunning(ctx context.Context) (bool, error) {
	var units []listedUnit
	call := s.conn.Object(systemdName, systemdPath).CallWithContext(ctx, systemdManager+".ListUnitsByNames", 0, PackageTaskUnits)
	if err := call.Store(&units); err != nil {
		return false, err
	}
	// A oneshot unit is "activating" while it runs.
	return slices.ContainsFunc(units, func(unit listedUnit) bool {
		return unit.ActiveState == "activating" || unit.ActiveState == "active" || unit.ActiveState == "deactivating"
	}), nil
}

// PackageTaskChanges signals that a package task unit started or ended.
func (s *Systemd) PackageTaskChanges() <-chan struct{} { return s.tasks }

// unitPath is the D-Bus object path of a unit: systemd escapes every byte
// that is not a letter or digit as _xx.
func unitPath(name string) dbus.ObjectPath {
	var escaped strings.Builder
	for _, b := range []byte(name) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			escaped.WriteByte(b)
		} else {
			fmt.Fprintf(&escaped, "_%02x", b)
		}
	}
	return dbus.ObjectPath(systemdPath + "/unit/" + escaped.String())
}

// watchPackageTasks publishes the flags group when a package task starts or
// ends. When one ends after a list refresh, it re-reads Available updates
// (v1 spec §4.6). Most hourly turns refresh nothing, so they read nothing.
func (c *Collector) watchPackageTasks(ctx context.Context, publish func(protocol.Groups)) {
	refreshed := c.lastRefresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.host.Tasks.PackageTaskChanges():
			flags := c.publishFlags(ctx, publish)
			if flags.PackageTaskRunning || !c.host.has(CapabilityAvailableUpdates) {
				continue
			}
			if last := c.lastRefresh(); !equal(last, refreshed) {
				refreshed = last
				publish(protocol.Groups{AvailableUpdates: c.availableUpdates(ctx)})
			}
		}
	}
}

func equal(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// lastRefresh is the time of the last successful package list refresh, or
// nil when there is none.
func (c *Collector) lastRefresh() *string {
	last, ok := helper.ReadPackageListStamp(filepath.Join(c.host.Root, helper.DefaultPackageListStamp))
	if !ok {
		return nil
	}
	return ptr(last.UTC().Format(time.RFC3339))
}
