package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

func encode(t *testing.T, message protocol.Message) []byte {
	t.Helper()
	frame, err := protocol.Encode(message)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// closedWithin says whether the Agent ends the connection within wait. Other
// messages before the end are skipped.
func closedWithin(t *testing.T, l *link, wait time.Duration) bool {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case <-l.messages:
		case err := <-l.err:
			l.err <- err
			return true
		case <-deadline:
			return false
		}
	}
}

// stillOpen says whether the connection still gets the next delta.
func stillOpen(t *testing.T, a *agent, l *link, cpu float64) bool {
	t.Helper()
	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(cpu)}})
	message, err := read(t, l, 5*time.Second)
	if err != nil {
		return false
	}
	_, ok := message.(*protocol.Delta)
	return ok
}

func TestPairingRemoveDeletesTheKeyAndClosesTheConnection(t *testing.T) {
	a := startAgent(t, nil)
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t)

	sendFrame(t, conn, encode(t, &protocol.PairingRemoveRequest{ID: "rm-1"}))
	message, err := read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reply, ok := message.(*protocol.PairingRemoveReply); !ok || reply.ReplyTo != "rm-1" {
		t.Fatalf("got %+v, want the pairing_remove reply", message)
	}
	if !closedWithin(t, conn, 5*time.Second) {
		t.Error("the connection stayed open after its Pairing was removed")
	}
	if _, resp, err := ha.dial(t, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("login after pairing_remove: err = %v, want 401", err)
	}
	if list, _ := a.server.Pairings.List(); len(list) != 0 {
		t.Errorf("Pairings = %+v, want none", list)
	}
}

func TestTwoPairingsWorkIndependently(t *testing.T) {
	a := startAgent(t, nil)
	home, _ := pair(t, a, a.newCode(t))
	office, _ := pair(t, a, a.newCode(t))
	homeConn, _, _ := home.connect(t)
	officeConn, _, _ := office.connect(t)

	sendFrame(t, homeConn, encode(t, &protocol.PairingRemoveRequest{ID: "rm-1"}))
	if !closedWithin(t, homeConn, 5*time.Second) {
		t.Fatal("the removed Pairing's connection stayed open")
	}
	if !stillOpen(t, a, officeConn, 11) {
		t.Error("the other Pairing's connection was closed")
	}
	if _, _, err := office.dial(t, nil); err != nil {
		t.Errorf("the other Pairing cannot log in: %v", err)
	}
}

// hostbeacon pairings remove runs as root in another process.
func TestOwnerRemoveClosesThatConnectionOnly(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.PairingCheckInterval = 50 * time.Millisecond })
	home, _ := pair(t, a, a.newCode(t))
	office, _ := pair(t, a, a.newCode(t))
	homeConn, _, _ := home.connect(t)
	homeConn2, _, _ := home.connect(t)
	officeConn, _, _ := office.connect(t)

	owner := pairing.Open(a.dir, time.Now)
	if _, err := owner.Remove(pairing.ID(home.key)); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(t, homeConn, 2*time.Second) || !closedWithin(t, homeConn2, 2*time.Second) {
		t.Fatal("a connection of the removed Pairing stayed open")
	}
	if !stillOpen(t, a, officeConn, 12) {
		t.Error("the other Pairing's connection was closed")
	}
}

func TestMessagesOverTheLimitCloseTheConnectionAlsoAfterReconnect(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.MessagesPerMinute = 4 })
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t) // the hello reply is message 1
	event := []byte(`{"type":"future_thing","id":"e","kind":"event"}`)
	sendFrame(t, conn, event)
	sendFrame(t, conn, event) // message 3
	if !stillOpen(t, a, conn, 13) {
		t.Fatal("closed below the limit")
	}
	conn.conn.CloseNow()

	conn, _, _ = ha.connect(t) // a new hello reply: message 4
	sendFrame(t, conn, event)  // message 5
	if !closedWithin(t, conn, 5*time.Second) {
		t.Error("the connection stayed open over the message limit")
	}
}

func TestActionsOverTheLimitCloseTheConnection(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.ActionsPerMinute = 2 })
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t)
	for i := range 3 {
		sendFrame(t, conn, encode(t, &protocol.ActionRequest{ID: "a" + string(rune('0'+i)), ActionID: identity.NewUUID(), Action: protocol.ActionReboot}))
	}
	if !closedWithin(t, conn, 5*time.Second) {
		t.Error("the connection stayed open over the Action limit")
	}
}

func TestOpenConnectionsPerPairingAreCapped(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.MaxConnectionsPerPairing = 2 })
	home, _ := pair(t, a, a.newCode(t))
	office, _ := pair(t, a, a.newCode(t))
	first, _, _ := home.connect(t)
	home.connect(t)
	if _, resp, err := home.dial(t, nil); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third connection: err = %v, want 429", err)
	}
	office.connect(t) // another Pairing has its own count

	first.conn.CloseNow()
	time.Sleep(100 * time.Millisecond)
	home.connect(t) // a slot is free again
}
