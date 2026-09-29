package server

import (
	"reflect"
	"sync"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// State holds the latest state groups. The samplers set them; each
// connection sends a delta when one changes.
type State struct {
	mu      sync.Mutex
	system  protocol.System
	changed chan struct{}
}

// NewState starts with the given system group.
func NewState(system protocol.System) *State {
	return &State{system: system, changed: make(chan struct{})}
}

// SetSystem replaces the system group. Nothing happens when the values are
// the same, so no delta is sent.
func (s *State) SetSystem(system protocol.System) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reflect.DeepEqual(system, s.system) {
		return
	}
	s.system = system
	close(s.changed)
	s.changed = make(chan struct{})
}

// System returns the system group and a channel that closes when it changes.
func (s *State) System() (protocol.System, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.system, s.changed
}
