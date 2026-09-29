// Package server is the Agent's network part: one TCP port with TLS 1.3,
// the Pairing step, and the WebSocket to each paired Home Assistant.
//
// Listening rules (v1 spec §5): only allowed source addresses; before a
// valid key, only the Pairing step, and only while a code is active; no
// request with a browser Origin header; caps and a short timeout for
// connections without a key. The log never holds a key or a Pairing code.
package server

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

const (
	defaultMaxUnauthenticated          = 64
	defaultMaxUnauthenticatedPerSource = 8
	defaultUnauthenticatedTimeout      = 10 * time.Second

	maxFrameBytes  = 64 << 10
	maxPairBytes   = 4 << 10
	maxPairingName = 64
	helloTimeout   = 10 * time.Second
	writeTimeout   = 10 * time.Second
	pairPath       = "/v1/pair"
	webSocketPath  = "/v1/ws"
	bearerPrefix   = "Bearer "
)

// Server is the Agent's network part.
type Server struct {
	Config   config.Config
	Identity identity.Identity
	Pairings *pairing.Pairings
	State    *State
	// Hello is sent after login. Serve fills in the message ID and the
	// protocol version.
	Hello protocol.HelloRequest
	// Actions passes Action requests to the root helper, which checks, logs,
	// and runs them. Without it, every Action is refused.
	Actions Actions
	// KnownID saves the Host ID Home Assistant sends in its hello reply, so
	// a copy of this Host can report it. Nil means it is not saved.
	KnownID func(hostID string) error
	Log     *slog.Logger

	// Limits for connections that have not logged in with a key. Zero means
	// the default.
	MaxUnauthenticated          int
	MaxUnauthenticatedPerSource int
	UnauthenticatedTimeout      time.Duration

	// Limits per Pairing, and how often the Pairings file is checked for
	// Pairings an owner command removed. Zero means the default.
	MaxConnectionsPerPairing int
	MessagesPerMinute        int
	ActionsPerMinute         int
	PairingCheckInterval     time.Duration
	// LastSeenInterval is how often the last seen time of a connected
	// Pairing is saved. Zero means the default.
	LastSeenInterval time.Duration

	sessions *sessions
}

// Actions is the root helper's Action job.
type Actions interface {
	Act(ctx context.Context, request helper.ActionRequest) (helper.Ack, <-chan protocol.ActionOutcome, error)
}

type connKey struct{}

// Serve answers connections on l until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.sessions = &sessions{
		maxConnections:    orDefault(s.MaxConnectionsPerPairing, defaultMaxConnectionsPerPairing),
		messagesPerMinute: orDefault(s.MessagesPerMinute, defaultMessagesPerMinute),
		actionsPerMinute:  orDefault(s.ActionsPerMinute, defaultActionsPerMinute),
		pairings:          map[string]*pairingState{},
	}
	go s.watchPairings(ctx, orDefault(s.PairingCheckInterval, defaultPairingCheckInterval), orDefault(s.LastSeenInterval, defaultLastSeenInterval))
	timeout := orDefault(s.UnauthenticatedTimeout, defaultUnauthenticatedTimeout)
	limited := &listener{
		Listener:  l,
		allows:    s.Config.Allows,
		max:       orDefault(s.MaxUnauthenticated, defaultMaxUnauthenticated),
		perSource: orDefault(s.MaxUnauthenticatedPerSource, defaultMaxUnauthenticatedPerSource),
		timeout:   timeout,
		bySource:  map[netip.Addr]int{},
		refused: func(source netip.Addr, reason string) {
			s.Log.Debug("refused a connection", "source", source, "reason", reason)
		},
	}
	tlsListener := tls.NewListener(limited, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.Identity.Certificate},
		NextProtos:   []string{"http/1.1"},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pairPath, s.handlePair)
	mux.HandleFunc("GET "+webSocketPath, s.handleWebSocket)
	server := &http.Server{
		Handler:           refuseOrigin(mux),
		ReadHeaderTimeout: timeout,
		ReadTimeout:       timeout,
		WriteTimeout:      timeout, // lifted by loggedIn before the WebSocket starts
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          slog.NewLogLogger(s.Log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if tlsConn, ok := conn.(*tls.Conn); ok {
				conn = tlsConn.NetConn()
			}
			return context.WithValue(ctx, connKey{}, conn)
		},
	}
	// One request per connection without a key: the Pairing step or the
	// WebSocket login.
	server.SetKeepAlivesEnabled(false)

	go func() {
		<-ctx.Done()
		server.Close()
	}()
	err := server.Serve(tlsListener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func orDefault[T int | time.Duration](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}

// refuseOrigin rejects any request with an Origin header: only browsers send
// one, and Home Assistant never does.
func refuseOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, found := r.Header["Origin"]; found {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type pairRequest struct {
	Name  string `json:"name"`
	Nonce []byte `json:"nonce"`
	Proof []byte `json:"proof"`
}

type pairResponse struct {
	InstanceID string `json:"instance_id"`
	Hostname   string `json:"hostname"`
	Key        []byte `json:"key"`
	Proof      []byte `json:"proof"`
	// CopiedFrom lets Home Assistant warn that this Host is a copy before
	// it Re-pairs a Host with it.
	CopiedFrom []string `json:"copied_from"`
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var request pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPairBytes)).Decode(&request); err != nil || !validName(request.Name) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, proof, err := s.Pairings.Pair(request.Name, s.Identity.Fingerprint[:], request.Nonce, request.Proof)
	if errors.Is(err, pairing.ErrRefused) {
		s.Log.Warn("refused a Pairing step: wrong, expired, used, or no active Pairing code", "source", r.RemoteAddr, "error", err)
		http.Error(w, "refused", http.StatusForbidden)
		return
	}
	if err != nil {
		s.Log.Error("cannot run the Pairing step", "error", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("paired with Home Assistant; waiting for its first login", "pairing", request.Name, "source", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pairResponse{InstanceID: s.Identity.InstanceID, Hostname: s.hostname(), Key: key, Proof: proof, CopiedFrom: s.Hello.CopiedFrom})
}

// hostname is the current hostname from the agent group.
func (s *Server) hostname() string {
	if groups, _ := s.State.Groups(); groups.Agent != nil {
		return groups.Agent.Hostname
	}
	return s.Hello.Hostname
}

// validName accepts a Pairing name of printable text, up to 64 characters.
func validName(name string) bool {
	runes := []rune(name)
	return len(runes) > 0 && len(runes) <= maxPairingName && !slices.ContainsFunc(runes, func(r rune) bool { return !unicode.IsPrint(r) })
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	header := r.Header.Get("Authorization")
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, bearerPrefix))
	if !strings.HasPrefix(header, bearerPrefix) || err != nil {
		key = nil
	}
	login, ok, err := s.Pairings.Login(key)
	if err != nil {
		s.Log.Error("cannot check the Pairing key; refusing the connection", "error", err)
		http.Error(w, "error", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		s.Log.Warn("refused a connection with an unknown Pairing key", "source", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.sessions.open(login.ID) {
		s.Log.Warn("refused a connection: too many open connections for this Pairing", "pairing", login.Name, "source", r.RemoteAddr)
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	if conn, found := r.Context().Value(connKey{}).(*countedConn); found {
		conn.loggedIn()
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.sessions.closed(login.ID, nil)
		s.Log.Warn("WebSocket upgrade failed", "source", r.RemoteAddr, "error", err)
		return
	}
	s.sessions.started(login.ID, ws)
	defer s.sessions.closed(login.ID, ws)
	// The Pairing may have been removed since Login, before the watcher could
	// see this connection. From now on the watcher sees it, so check once more.
	if _, err := s.Pairings.Find(login.ID); err != nil {
		ws.Close(websocket.StatusPolicyViolation, "Pairing removed")
		return
	}
	s.Log.Info("Home Assistant connected", "pairing", login.Name, "source", r.RemoteAddr)
	err = s.session(r.Context(), ws, login)
	s.Log.Info("Home Assistant disconnected", "pairing", login.Name, "reason", err)
}

// session runs one logged-in connection: hello, then a snapshot, then a
// delta each time the state changes.
func (s *Server) session(ctx context.Context, ws *websocket.Conn, login pairing.Pairing) error {
	defer ws.CloseNow()
	ws.SetReadLimit(maxFrameBytes)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hello := s.Hello
	hello.Hostname = s.hostname()
	hello.ID = identity.NewUUID()
	hello.ProtocolVersion = protocol.Version
	hello.ProtocolMajors = protocol.Majors
	if err := send(ctx, ws, &hello); err != nil {
		return err
	}

	replies := make(chan *protocol.HelloReply, 1)
	readDone := make(chan error, 1)
	go func() { readDone <- s.read(ctx, ws, login, hello.ID, replies) }()

	select {
	case reply := <-replies:
		if !slices.ContainsFunc(reply.ProtocolMajors, func(major int) bool { return slices.Contains(protocol.Majors, major) }) {
			ws.Close(websocket.StatusPolicyViolation, "no common protocol major")
			return errors.New("no common protocol major")
		}
		if reply.HostID != nil && s.KnownID != nil {
			if err := s.KnownID(*reply.HostID); err != nil {
				s.Log.Error("cannot save the Host ID Home Assistant knows this Agent as", "error", err)
			}
		}
	case err := <-readDone:
		return err
	case <-time.After(helloTimeout):
		ws.Close(websocket.StatusPolicyViolation, "no hello reply")
		return errors.New("no hello reply")
	}

	groups, changed := s.State.Groups()
	sent := snapshotGroups(hello, groups)
	if err := send(ctx, ws, &protocol.Snapshot{ID: identity.NewUUID(), Groups: sent}); err != nil {
		return err
	}
	for {
		select {
		case err := <-readDone:
			return err
		case <-changed:
			groups, changed = s.State.Groups()
			delta, ok := changedGroups(sent, groups)
			if !ok {
				continue
			}
			if err := send(ctx, ws, &protocol.Delta{ID: identity.NewUUID(), Groups: delta}); err != nil {
				return err
			}
			merge(&sent, delta)
		}
	}
}

// snapshotGroups fills in the groups a snapshot needs that the state does
// not hold yet.
func snapshotGroups(hello protocol.HelloRequest, groups protocol.Groups) protocol.Groups {
	if groups.Agent == nil {
		groups.Agent = &protocol.AgentInfo{
			Hostname:           hello.Hostname,
			AgentVersion:       hello.AgentVersion,
			NewestAgentVersion: hello.NewestAgentVersion,
			Capabilities:       hello.Capabilities,
			EnabledActions:     hello.EnabledActions,
		}
	}
	if groups.System == nil {
		groups.System = &protocol.System{}
	}
	if groups.UpdateRun == nil {
		// The Update run is not read yet.
		groups.UpdateRun = &protocol.UpdateRun{State: "idle", NeedsManualUpdate: protocol.NameList{Names: []string{}}}
	}
	if groups.Flags == nil {
		groups.Flags = &protocol.Flags{RebootRequired: "unknown"}
	}
	return groups
}

// read handles the messages Home Assistant sends until the connection ends.
func (s *Server) read(ctx context.Context, ws *websocket.Conn, login pairing.Pairing, helloID string, replies chan<- *protocol.HelloReply) error {
	for {
		messageType, frame, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			ws.Close(websocket.StatusUnsupportedData, "text frames only")
			return errors.New("got a binary frame")
		}
		message, err := protocol.Decode(frame)
		_, action := message.(*protocol.ActionRequest)
		if !s.sessions.allowMessage(login.ID, action) {
			s.Log.Warn("closed a connection: too many messages or Actions for this Pairing", "pairing", login.Name)
			ws.Close(websocket.StatusPolicyViolation, "rate limit")
			return errors.New("rate limit")
		}
		if err != nil {
			s.Log.Warn("ignored a malformed message", "error", err)
			continue
		}
		switch m := message.(type) {
		case *protocol.HelloReply:
			if m.ReplyTo == helloID {
				select {
				case replies <- m:
				default:
				}
			}
		case *protocol.PairingRemoveRequest:
			return s.removePairing(ctx, ws, login, m)
		case *protocol.ActionRequest:
			go s.action(ctx, ws, login, m)
		case *protocol.Unknown:
			if reply, ok := protocol.UnsupportedReply(m, identity.NewUUID()); ok {
				if err := send(ctx, ws, reply); err != nil {
					return err
				}
			}
		}
	}
}

// action passes an Action request to the root helper and sends its Ack, then
// the result of an accepted Action. When the helper cannot be asked, nothing
// is logged, so the Action is refused as cannot_log.
func (s *Server) action(ctx context.Context, ws *websocket.Conn, login pairing.Pairing, request *protocol.ActionRequest) {
	ack := helper.Ack{Status: "refused", Reason: ptr(protocol.ReasonCannotLog)}
	var result <-chan protocol.ActionOutcome
	if s.Actions == nil {
		s.Log.Error("refused an Action: the root helper is not set up", "action", request.Action)
	} else {
		answer, outcomes, err := s.Actions.Act(ctx, helper.ActionRequest{
			ActionID:    request.ActionID,
			Action:      request.Action,
			PairingID:   login.ID,
			PairingName: login.Name,
			User:        request.User,
		})
		if err != nil {
			s.Log.Error("refused an Action: cannot ask the root helper", "action", request.Action, "error", err)
		} else {
			ack, result = answer, outcomes
		}
	}
	s.Log.Info("Action request", "action", request.Action, "action_id", request.ActionID, "pairing", login.Name, "status", ack.Status, "reason", ack.Reason)
	if err := send(ctx, ws, &protocol.ActionAck{
		ID:          identity.NewUUID(),
		ReplyTo:     request.ID,
		ActionID:    request.ActionID,
		Status:      ack.Status,
		Reason:      ack.Reason,
		FirstResult: ack.FirstResult,
	}); err != nil || result == nil {
		return
	}
	select {
	case <-ctx.Done():
	case outcome, ok := <-result:
		if ok {
			send(ctx, ws, &protocol.ActionResult{ID: identity.NewUUID(), ActionID: request.ActionID, Action: request.Action, Result: outcome.Result, Error: outcome.Error})
		}
	}
}

func ptr[T any](value T) *T { return &value }

// removePairing deletes the Pairing this connection logged in with, confirms
// it, and closes every connection of that Pairing. Other Pairings go on.
func (s *Server) removePairing(ctx context.Context, ws *websocket.Conn, login pairing.Pairing, request *protocol.PairingRemoveRequest) error {
	if _, err := s.Pairings.Remove(login.ID); err != nil && !errors.Is(err, pairing.ErrNotFound) {
		s.Log.Error("cannot remove the Pairing Home Assistant asked to remove", "pairing", login.Name, "error", err)
		ws.Close(websocket.StatusInternalError, "cannot remove the Pairing")
		return err
	}
	s.Log.Info("Home Assistant removed its Pairing", "pairing", login.Name, "id", login.ID)
	if err := send(ctx, ws, &protocol.PairingRemoveReply{ID: identity.NewUUID(), ReplyTo: request.ID}); err != nil {
		return err
	}
	s.sessions.closePairing(login.ID)
	return errors.New("Pairing removed")
}

func send(ctx context.Context, ws *websocket.Conn, message protocol.Message) error {
	frame, err := protocol.Encode(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return ws.Write(ctx, websocket.MessageText, frame)
}
