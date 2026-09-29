package server

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
)

// Limits per Pairing (v1 spec §5). They are generous: Home Assistant sends
// only a few messages. Message and Action counts live as long as the Agent
// runs, so reconnecting does not reset them.
const (
	defaultMaxConnectionsPerPairing = 4
	defaultMessagesPerMinute        = 600
	defaultActionsPerMinute         = 10
	defaultPairingCheckInterval     = time.Second
	defaultLastSeenInterval         = time.Hour
)

// bucket allows perMinute events a minute, and up to perMinute at once.
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) take(perMinute int, now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = float64(perMinute)
	} else {
		b.tokens = min(float64(perMinute), b.tokens+now.Sub(b.last).Minutes()*float64(perMinute))
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// pairingState is what the Agent keeps for one Pairing while it runs.
type pairingState struct {
	// reserved counts logins whose WebSocket is not accepted yet.
	reserved    int
	connections map[*websocket.Conn]struct{}
	// removed is set once the Pairing's connections are being closed.
	removed  bool
	messages bucket
	actions  bucket
}

// sessions holds the open connections and the rate limits of each Pairing.
type sessions struct {
	maxConnections    int
	messagesPerMinute int
	actionsPerMinute  int

	mu       sync.Mutex
	pairings map[string]*pairingState
}

func (s *sessions) state(id string) *pairingState {
	state, found := s.pairings[id]
	if !found {
		state = &pairingState{connections: map[*websocket.Conn]struct{}{}}
		s.pairings[id] = state
	}
	return state
}

// open reserves a connection slot for the Pairing. It returns false when the
// Pairing has too many open connections.
func (s *sessions) open(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(id)
	if state.reserved+len(state.connections) >= s.maxConnections {
		return false
	}
	state.reserved++
	return true
}

// started swaps the reserved slot for the accepted WebSocket.
func (s *sessions) started(id string, ws *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(id)
	state.reserved--
	state.connections[ws] = struct{}{}
}

// closed frees the slot of ws, or a reserved slot when ws is nil.
func (s *sessions) closed(id string, ws *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(id)
	if ws == nil {
		state.reserved--
	} else {
		delete(state.connections, ws)
	}
}

// allowMessage counts one message from the Pairing, and one Action if action.
func (s *sessions) allowMessage(id string, action bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	state := s.state(id)
	if !state.messages.take(s.messagesPerMinute, now) {
		return false
	}
	return !action || state.actions.take(s.actionsPerMinute, now)
}

// closeRemoved closes every connection whose Pairing is not in kept, and
// returns the IDs of the Pairings whose connections it closed.
func (s *sessions) closeRemoved(kept []pairing.Pairing) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var closed []string
	for id, state := range s.pairings {
		found := false
		for _, p := range kept {
			found = found || p.ID == id
		}
		if !found && !state.removed {
			if len(state.connections) > 0 {
				closed = append(closed, id)
			}
			s.closePairingLocked(id)
		}
	}
	return closed
}

// closePairing closes every connection of the Pairing.
func (s *sessions) closePairing(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closePairingLocked(id)
}

func (s *sessions) closePairingLocked(id string) {
	state, found := s.pairings[id]
	if !found || state.removed {
		return
	}
	state.removed = true
	for ws := range state.connections {
		go ws.Close(websocket.StatusPolicyViolation, "Pairing removed")
	}
	if state.reserved+len(state.connections) == 0 {
		delete(s.pairings, id)
	}
}

// connected returns the IDs of the Pairings with an open connection.
func (s *sessions) connected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, state := range s.pairings {
		if len(state.connections) > 0 && !state.removed {
			ids = append(ids, id)
		}
	}
	return ids
}

// watchPairings closes the connections of a Pairing soon after an owner
// command removes it: hostbeacon pairings remove runs in another process. It
// also saves the last seen time of connected Pairings every seenInterval, so
// a Home Assistant that stays connected is never reported as unseen.
func (s *Server) watchPairings(ctx context.Context, interval, seenInterval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastSaved := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			connected := s.sessions.connected()
			if len(connected) == 0 {
				continue
			}
			if time.Since(lastSaved) >= seenInterval {
				lastSaved = time.Now()
				if err := s.Pairings.Seen(connected); err != nil {
					s.Log.Error("cannot save the last seen time of the Pairings", "error", err)
				}
			}
			list, err := s.Pairings.List()
			if err != nil {
				s.Log.Error("cannot read the Pairings", "error", err)
				continue
			}
			for _, id := range s.sessions.closeRemoved(list) {
				s.Log.Info("closed the connections of a removed Pairing", "id", id)
			}
		}
	}
}
