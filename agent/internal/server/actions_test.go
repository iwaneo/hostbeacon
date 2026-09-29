package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// fakeActions stands in for the root helper.
type fakeActions struct {
	mu       sync.Mutex
	requests []helper.ActionRequest
	ack      helper.Ack
	err      error
	result   chan protocol.ActionOutcome
}

func (f *fakeActions) Act(_ context.Context, request helper.ActionRequest) (helper.Ack, <-chan protocol.ActionOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.err != nil || f.ack.Status != "accepted" {
		return f.ack, nil, f.err
	}
	return f.ack, f.result, nil
}

const actionID = "d4c3b2a1-9f8e-4d7c-b6a5-493827160a5b"

func sendAction(t *testing.T, l *link, user *string) {
	t.Helper()
	frame, err := protocol.Encode(&protocol.ActionRequest{ID: "ha-7", ActionID: actionID, Action: protocol.ActionReboot, User: user})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, l, frame)
}

func readAck(t *testing.T, l *link) *protocol.ActionAck {
	t.Helper()
	message, err := read(t, l, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := message.(*protocol.ActionAck)
	if !ok || ack.ReplyTo != "ha-7" || ack.ActionID != actionID {
		t.Fatalf("got %+v, want the action_ack for ha-7", message)
	}
	return ack
}

func TestActionRequestGoesToTheHelperWithThePairingAndUser(t *testing.T) {
	actions := &fakeActions{ack: helper.Ack{Status: "accepted"}, result: make(chan protocol.ActionOutcome, 1)}
	a := startAgent(t, func(s *Server) { s.Actions = actions })
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t)

	sendAction(t, conn, ptr("admin"))
	if ack := readAck(t, conn); ack.Status != "accepted" || ack.Reason != nil {
		t.Errorf("ack = %+v", ack)
	}
	actions.mu.Lock()
	got := actions.requests
	actions.mu.Unlock()
	list, _ := a.server.Pairings.List()
	if len(got) != 1 || got[0].ActionID != actionID || got[0].Action != protocol.ActionReboot ||
		got[0].PairingID != list[0].ID || got[0].PairingName != "Home" || got[0].User == nil || *got[0].User != "admin" {
		t.Errorf("helper got %+v", got)
	}

	actions.result <- protocol.ActionOutcome{Result: "failed", Error: ptr("systemctl reboot exited with status 1")}
	message, err := read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := message.(*protocol.ActionResult)
	if !ok || result.ActionID != actionID || result.Action != protocol.ActionReboot || result.Result != "failed" || *result.Error != "systemctl reboot exited with status 1" {
		t.Errorf("got %+v, want the action_result", message)
	}
}

func TestActionRefusalIsPassedOn(t *testing.T) {
	duplicate := protocol.ReasonDuplicate
	actions := &fakeActions{ack: helper.Ack{Status: "refused", Reason: &duplicate, FirstResult: &protocol.ActionOutcome{Result: "ok"}}}
	a := startAgent(t, func(s *Server) { s.Actions = actions })
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t)

	sendAction(t, conn, nil)
	ack := readAck(t, conn)
	if ack.Status != "refused" || *ack.Reason != protocol.ReasonDuplicate || ack.FirstResult == nil || ack.FirstResult.Result != "ok" {
		t.Errorf("ack = %+v", ack)
	}
	if actions.requests[0].User != nil {
		t.Errorf("user = %v, want none", *actions.requests[0].User)
	}
	if message, err := read(t, conn, 300*time.Millisecond); err == nil {
		t.Errorf("got %T after a refusal, want nothing", message)
	}
}

func TestActionIsRefusedAsCannotLogWhenTheHelperFails(t *testing.T) {
	for name, actions := range map[string]Actions{
		"helper error": &fakeActions{err: errors.New("dial unix: no such file")},
		"no helper":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			a := startAgent(t, func(s *Server) { s.Actions = actions })
			ha, _ := pair(t, a, a.newCode(t))
			conn, _, _ := ha.connect(t)

			sendAction(t, conn, ptr("admin"))
			if ack := readAck(t, conn); ack.Status != "refused" || *ack.Reason != protocol.ReasonCannotLog {
				t.Errorf("ack = %+v, want cannot_log", ack)
			}
		})
	}
}
