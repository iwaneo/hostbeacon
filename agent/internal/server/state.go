package server

import (
	"reflect"
	"sync"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// State holds the latest state groups. The collector sets them; each
// connection sends a delta with the groups that changed. It also holds the
// result of the last Agent update, which each connection sends once.
type State struct {
	mu          sync.Mutex
	groups      protocol.Groups
	agentUpdate *protocol.ActionResult
	changed     chan struct{}
}

// NewState starts with the given groups.
func NewState(groups protocol.Groups) *State {
	return &State{groups: groups, changed: make(chan struct{})}
}

// Set replaces each group that is not nil. Nothing happens when the values
// are the same, so no delta is sent.
func (s *State) Set(groups protocol.Groups) {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := s.groups
	if !merge(&merged, groups) {
		return
	}
	s.groups = merged
	close(s.changed)
	s.changed = make(chan struct{})
}

// SetAgentUpdateResult sets the result of the last Agent update Home
// Assistant asked for; nil when there is none to send.
func (s *State) SetAgentUpdateResult(result *protocol.ActionResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reflect.DeepEqual(result, s.agentUpdate) {
		return
	}
	s.agentUpdate = result
	close(s.changed)
	s.changed = make(chan struct{})
}

// AgentUpdateResult is the result of the last Agent update, or nil.
func (s *State) AgentUpdateResult() *protocol.ActionResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentUpdate
}

// Groups returns the groups and a channel that closes when one changes.
func (s *State) Groups() (protocol.Groups, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groups, s.changed
}

// changedGroups returns the groups in current whose values differ from
// sent. Each group is complete.
func changedGroups(sent, current protocol.Groups) (protocol.Groups, bool) {
	var delta protocol.Groups
	sentValue, currentValue, deltaValue := reflect.ValueOf(sent), reflect.ValueOf(current), reflect.ValueOf(&delta).Elem()
	changed := false
	for i := range currentValue.NumField() {
		if group := currentValue.Field(i); !group.IsNil() && !reflect.DeepEqual(group.Interface(), sentValue.Field(i).Interface()) {
			deltaValue.Field(i).Set(group)
			changed = true
		}
	}
	return delta, changed
}

// merge sets each group of update that is not nil into groups, and says
// whether a value changed.
func merge(groups *protocol.Groups, update protocol.Groups) bool {
	delta, changed := changedGroups(*groups, update)
	deltaValue, groupsValue := reflect.ValueOf(delta), reflect.ValueOf(groups).Elem()
	for i := range deltaValue.NumField() {
		if group := deltaValue.Field(i); !group.IsNil() {
			groupsValue.Field(i).Set(group)
		}
	}
	return changed
}
