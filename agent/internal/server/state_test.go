package server

import (
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

func TestStateSignalsOnlyRealChanges(t *testing.T) {
	state := NewState(protocol.Groups{System: &protocol.System{CPUPercent: ptr(5.0)}})
	_, changed := state.Groups()

	// Same values in a new group value: no change.
	state.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(5.0)}})
	select {
	case <-changed:
		t.Fatal("changed without a new value")
	default:
	}

	state.Set(protocol.Groups{Disks: &protocol.Disks{Mounts: []protocol.Mount{{Mount: "/"}}}})
	select {
	case <-changed:
	default:
		t.Fatal("no change signal for a new group")
	}
	groups, _ := state.Groups()
	if groups.System == nil || *groups.System.CPUPercent != 5 || groups.Disks == nil {
		t.Errorf("groups = %+v, want the system group kept and the disks group added", groups)
	}
}

func TestChangedGroupsHoldsOnlyChangedGroups(t *testing.T) {
	sent := protocol.Groups{
		System:  &protocol.System{CPUPercent: ptr(5.0)},
		Network: &protocol.Network{Interfaces: []protocol.Interface{{Name: "eth0"}}},
	}
	current := protocol.Groups{
		System:  &protocol.System{CPUPercent: ptr(5.0)},
		Network: &protocol.Network{Interfaces: []protocol.Interface{{Name: "eth0"}, {Name: "eth1"}}},
		Flags:   &protocol.Flags{RebootRequired: "yes"},
	}
	delta, changed := changedGroups(sent, current)
	if !changed || delta.System != nil || delta.Network == nil || len(delta.Network.Interfaces) != 2 || delta.Flags == nil {
		t.Errorf("delta = %+v, want the complete network group and flags only", delta)
	}
	if _, changed := changedGroups(current, current); changed {
		t.Error("no groups changed, but changedGroups says so")
	}
}
