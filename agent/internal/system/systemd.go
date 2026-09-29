package system

import (
	"context"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	systemdName    = "org.freedesktop.systemd1"
	systemdPath    = "/org/freedesktop/systemd1"
	systemdManager = "org.freedesktop.systemd1.Manager"
	systemdUnit    = "org.freedesktop.systemd1.Unit"
)

// listedUnit is one unit as ListUnits gives it.
type listedUnit struct {
	Name, Description, LoadState, ActiveState, SubState, Following string
	Path                                                           dbus.ObjectPath
	JobID                                                          uint32
	JobType                                                        string
	JobPath                                                        dbus.ObjectPath
}

// Systemd reads failed services and package tasks from systemd over the
// system D-Bus. Reading needs no root.
type Systemd struct {
	conn    *dbus.Conn
	changes chan struct{}
	// tasks gets a change of a package task unit.
	tasks     chan struct{}
	taskUnits map[dbus.ObjectPath]bool
}

// ConnectSystemd connects to the system D-Bus and subscribes to unit
// changes. It fails where systemd cannot be reached.
func ConnectSystemd(ctx context.Context) (*Systemd, error) {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	s := &Systemd{conn: conn, changes: make(chan struct{}, 1), tasks: make(chan struct{}, 1), taskUnits: map[dbus.ObjectPath]bool{}}
	for _, name := range PackageTaskUnits {
		s.taskUnits[unitPath(name)] = true
	}
	// systemd sends unit signals only while a client is subscribed.
	if err := conn.Object(systemdName, systemdPath).CallWithContext(ctx, systemdManager+".Subscribe", 0).Err; err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.AddMatchSignalContext(ctx,
		dbus.WithMatchSender(systemdName),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchArg(0, systemdUnit),
	); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := s.Failed(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	signals := make(chan *dbus.Signal, 64)
	conn.Signal(signals)
	go s.forward(signals)
	return s, nil
}

// forward turns each change of a unit's active state into one pending
// change, and one pending package task change for a package task unit.
// Changes that come while one is pending are merged.
func (s *Systemd) forward(signals <-chan *dbus.Signal) {
	for signal := range signals {
		if len(signal.Body) < 2 {
			continue
		}
		if changed, ok := signal.Body[1].(map[string]dbus.Variant); ok {
			if _, ok := changed["ActiveState"]; !ok {
				continue
			}
		}
		select {
		case s.changes <- struct{}{}:
		default:
		}
		if s.taskUnits[signal.Path] {
			select {
			case s.tasks <- struct{}{}:
			default:
			}
		}
	}
}

// Changes signals that a unit's active state changed.
func (s *Systemd) Changes() <-chan struct{} { return s.changes }

// Failed lists the names of the failed services.
func (s *Systemd) Failed(ctx context.Context) ([]string, error) {
	var units []listedUnit
	call := s.conn.Object(systemdName, systemdPath).CallWithContext(ctx, systemdManager+".ListUnitsFiltered", 0, []string{"failed"})
	if err := call.Store(&units); err != nil {
		return nil, err
	}
	names := []string{}
	for _, unit := range units {
		if strings.HasSuffix(unit.Name, ".service") {
			names = append(names, unit.Name)
		}
	}
	return names, nil
}

// Close ends the D-Bus connection.
func (s *Systemd) Close() error { return s.conn.Close() }
