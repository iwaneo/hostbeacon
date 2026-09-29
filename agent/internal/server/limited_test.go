package server

import (
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// connectLimited logs in and answers hello with a protocol major the Agent
// does not speak.
func (ha *homeAssistant) connectLimited(t *testing.T) (*link, *protocol.HelloRequest) {
	t.Helper()
	ws, _, err := ha.dial(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := listen(t, ws)
	message, err := read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hello := message.(*protocol.HelloRequest)
	reply, _ := protocol.Encode(&protocol.HelloReply{ID: "r1", ReplyTo: hello.ID, IntegrationVersion: "9", ProtocolVersion: "9.0", ProtocolMajors: []int{9}})
	sendFrame(t, conn, reply)
	return conn, hello
}

func TestNoCommonProtocolMajorIsLimitedMode(t *testing.T) {
	actions := &fakeActions{ack: helper.Ack{Status: "accepted"}}
	a := startAgent(t, func(s *Server) { s.Actions = actions })
	ha, _ := pair(t, a, a.newCode(t))
	conn, _ := ha.connectLimited(t)

	// No state: no snapshot, and no delta when the state changes.
	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(99.0)}})
	if message, err := read(t, conn, 300*time.Millisecond); err == nil {
		t.Fatalf("got %T in limited mode", message)
	}
	// Only the agent_update Action: a Reboot request is not answered.
	sendAction(t, conn, nil)
	if message, err := read(t, conn, 300*time.Millisecond); err == nil {
		t.Fatalf("got %T for a Reboot in limited mode", message)
	}
	frame, _ := protocol.Encode(&protocol.ActionRequest{ID: "ha-7", ActionID: actionID, Action: protocol.ActionAgentUpdate})
	sendFrame(t, conn, frame)
	if ack := readAck(t, conn); ack.Status != "accepted" {
		t.Fatalf("ack %+v", ack)
	}
	if len(actions.requests) != 1 || actions.requests[0].Action != protocol.ActionAgentUpdate {
		t.Fatalf("helper got %+v", actions.requests)
	}

	// The update's result, and pairing_remove, still work.
	a.server.State.SetAgentUpdateResult(&protocol.ActionResult{ActionID: actionID, Action: protocol.ActionAgentUpdate, Result: "ok"})
	if message, err := read(t, conn, 5*time.Second); err != nil {
		t.Fatal(err)
	} else if result, ok := message.(*protocol.ActionResult); !ok || result.ActionID != actionID {
		t.Fatalf("got %+v, want the Agent update result", message)
	}
	frame, _ = protocol.Encode(&protocol.PairingRemoveRequest{ID: "rm-1"})
	sendFrame(t, conn, frame)
	if message, err := read(t, conn, 5*time.Second); err != nil {
		t.Fatal(err)
	} else if _, ok := message.(*protocol.PairingRemoveReply); !ok {
		t.Fatalf("got %T, want pairing_remove reply", message)
	}
}

func TestAgentUpdateResultIsSentOnceToEachConnection(t *testing.T) {
	a := startAgent(t, nil)
	ha, _ := pair(t, a, a.newCode(t))
	first, _, _ := ha.connect(t)

	result := &protocol.ActionResult{ActionID: actionID, Action: protocol.ActionAgentUpdate, Result: "failed", Error: ptr("rolled back")}
	a.server.State.SetAgentUpdateResult(result)
	wantResult := func(conn *link) {
		t.Helper()
		message, err := read(t, conn, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := message.(*protocol.ActionResult); !ok || got.ActionID != actionID || got.Result != "failed" || *got.Error != "rolled back" {
			t.Fatalf("got %+v, want the Agent update result", message)
		}
	}
	wantResult(first)
	// A connection made after the update, for example after the Agent
	// restarted, gets it after the snapshot.
	second, _, _ := ha.connect(t)
	wantResult(second)

	a.server.State.SetAgentUpdateResult(result)
	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(99.0)}})
	if message, _ := read(t, first, 5*time.Second); message == nil {
		t.Fatal("no delta")
	} else if _, ok := message.(*protocol.Delta); !ok {
		t.Fatalf("got %T, want only the delta: the result was sent already", message)
	}
}

func TestHelloHasTheNewestAgentVersion(t *testing.T) {
	a := startAgent(t, nil)
	a.server.State.Set(protocol.Groups{Agent: &protocol.AgentInfo{Hostname: "test-host", AgentVersion: "0.0.0", NewestAgentVersion: ptr("1.2.3"),
		Capabilities: []string{}, EnabledActions: []protocol.Action{}}})
	ha, _ := pair(t, a, a.newCode(t))
	_, hello, _ := ha.connect(t)
	if hello.NewestAgentVersion == nil || *hello.NewestAgentVersion != "1.2.3" {
		t.Fatalf("hello has newest %v", hello.NewestAgentVersion)
	}
}
