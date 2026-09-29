package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

func ptr[T any](v T) *T { return &v }

// logBuffer collects the Agent's log for checks.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// agent is a running Agent network part on 127.0.0.1.
type agent struct {
	server  *Server
	dir     string
	address string
	log     *logBuffer
}

func startAgent(t *testing.T, change func(*Server)) *agent {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := &logBuffer{}
	s := &Server{
		Config:   config.Config{Port: 0, AllowedSources: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}},
		Identity: id,
		Pairings: pairing.Open(dir, time.Now),
		State:    NewState(protocol.Groups{System: &protocol.System{CPUPercent: ptr(5.0), MemoryPercent: ptr(40.0)}}),
		Hello: protocol.HelloRequest{
			InstanceID:     id.InstanceID,
			RunID:          identity.NewUUID(),
			CopiedFrom:     []string{},
			Hostname:       "test-host",
			AgentVersion:   "0.0.0",
			Capabilities:   []string{},
			EnabledActions: []protocol.Action{},
			Architecture:   "amd64",
		},
		Log: slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if change != nil {
		change(s)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Serve(ctx, listener)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &agent{server: s, dir: dir, address: listener.Addr().String(), log: log}
}

func (a *agent) newCode(t *testing.T) string {
	t.Helper()
	code, _, err := pairing.NewCode(a.dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// homeAssistant is the test client that plays Home Assistant.
type homeAssistant struct {
	agent       *agent
	fingerprint []byte
	key         []byte
}

// fetchFingerprint reads the certificate the Agent shows, without trusting it.
func fetchFingerprint(t *testing.T, address string) []byte {
	t.Helper()
	conn, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return sum[:]
}

// client pins the certificate fingerprint.
func pinnedClient(fingerprint []byte) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if sum := sha256.Sum256(raw[0]); !bytes.Equal(sum[:], fingerprint) {
				return errors.New("certificate does not match the pin")
			}
			return nil
		},
	}}}
}

// pair runs the Pairing step with code. It returns the HTTP status.
func pair(t *testing.T, a *agent, code string) (*homeAssistant, int) {
	t.Helper()
	fingerprint := fetchFingerprint(t, a.address)
	nonce := make([]byte, 32)
	rand.Read(nonce)
	codeKey := pairing.CodeKey(code, nonce)
	body, _ := json.Marshal(map[string]any{
		"name":  "Home",
		"nonce": nonce,
		"proof": pairing.HomeAssistantProof(codeKey, fingerprint, nonce),
	})
	resp, err := pinnedClient(fingerprint).Post("https://"+a.address+"/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var answer pairResponse
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(answer.Proof, pairing.AgentProof(codeKey, fingerprint, nonce, answer.Key)) {
		t.Fatal("the Agent's proof does not match")
	}
	if answer.InstanceID != a.server.Identity.InstanceID || answer.Hostname != "test-host" {
		t.Errorf("pair answer = %s, %s; want the instance ID and hostname", answer.InstanceID, answer.Hostname)
	}
	return &homeAssistant{agent: a, fingerprint: fingerprint, key: answer.Key}, resp.StatusCode
}

func (ha *homeAssistant) dial(t *testing.T, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	if header.Get("Authorization") == "" {
		header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(ha.key))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "wss://"+ha.agent.address+"/v1/ws", &websocket.DialOptions{
		HTTPClient: pinnedClient(ha.fingerprint),
		HTTPHeader: header,
	})
}

// link is a WebSocket read in the background: coder/websocket closes the
// connection when a read times out, and some tests wait for nothing to come.
type link struct {
	conn     *websocket.Conn
	messages chan protocol.Message
	err      chan error
}

func listen(t *testing.T, conn *websocket.Conn) *link {
	t.Helper()
	l := &link{conn: conn, messages: make(chan protocol.Message, 16), err: make(chan error, 1)}
	t.Cleanup(func() { conn.CloseNow() })
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				l.err <- err
				return
			}
			message, err := protocol.Decode(data)
			if err != nil {
				l.err <- err
				return
			}
			l.messages <- message
		}
	}()
	return l
}

// read returns the next message, or an error when the connection ends or
// nothing comes within timeout.
func read(t *testing.T, l *link, timeout time.Duration) (protocol.Message, error) {
	t.Helper()
	select {
	case message := <-l.messages:
		return message, nil
	case err := <-l.err:
		l.err <- err
		return nil, err
	case <-time.After(timeout):
		return nil, errors.New("timeout")
	}
}

func sendFrame(t *testing.T, l *link, frame []byte) {
	t.Helper()
	if err := l.conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
}

// connect logs in, answers hello, and reads the snapshot.
func (ha *homeAssistant) connect(t *testing.T) (*link, *protocol.HelloRequest, *protocol.Snapshot) {
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
	hello, ok := message.(*protocol.HelloRequest)
	if !ok {
		t.Fatalf("first message is %T, want hello", message)
	}
	reply, err := protocol.Encode(&protocol.HelloReply{ID: "r1", ReplyTo: hello.ID, IntegrationVersion: "0.0.0", ProtocolVersion: "1.0", ProtocolMajors: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, conn, reply)
	message, err = read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := message.(*protocol.Snapshot)
	if !ok {
		t.Fatalf("message after hello is %T, want snapshot", message)
	}
	return conn, hello, snapshot
}

func TestPairThenHelloSnapshotAndDeltaOnlyAfterChange(t *testing.T) {
	a := startAgent(t, nil)
	ha, status := pair(t, a, a.newCode(t))
	if status != http.StatusOK {
		t.Fatalf("pair status = %d", status)
	}
	conn, hello, snapshot := ha.connect(t)

	if hello.InstanceID != a.server.Identity.InstanceID || hello.Hostname != "test-host" || hello.ProtocolVersion != protocol.Version {
		t.Errorf("hello = %+v", hello)
	}
	if got := snapshot.Groups.System; got == nil || *got.CPUPercent != 5 || *got.MemoryPercent != 40 {
		t.Errorf("snapshot system = %+v, want CPU 5 and memory 40", got)
	}
	if snapshot.Groups.Agent.Hostname != "test-host" || snapshot.Groups.UpdateRun.State != "idle" {
		t.Errorf("snapshot agent or update run = %+v %+v", snapshot.Groups.Agent, snapshot.Groups.UpdateRun)
	}

	// The same values again: no delta.
	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(5.0), MemoryPercent: ptr(40.0)}})
	if message, err := read(t, conn, 300*time.Millisecond); err == nil {
		t.Fatalf("got %T without a change, want nothing", message)
	}

	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(7.0), MemoryPercent: ptr(40.0)}})
	message, err := read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	delta, ok := message.(*protocol.Delta)
	if !ok {
		t.Fatalf("got %T, want delta", message)
	}
	if delta.Groups.System == nil || *delta.Groups.System.CPUPercent != 7 || delta.Groups.Agent != nil {
		t.Errorf("delta = %+v, want only the system group with CPU 7", delta.Groups)
	}
}

func TestEachConnectionGetsTheCurrentSnapshot(t *testing.T) {
	a := startAgent(t, nil)
	ha, _ := pair(t, a, a.newCode(t))
	first, _, _ := ha.connect(t)
	first.conn.CloseNow()
	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(9.0)}})
	_, _, snapshot := ha.connect(t)
	if *snapshot.Groups.System.CPUPercent != 9 {
		t.Errorf("snapshot CPU = %v, want 9", *snapshot.Groups.System.CPUPercent)
	}
}

func TestWrongCodeIsRefused(t *testing.T) {
	a := startAgent(t, nil)
	a.newCode(t)
	if _, status := pair(t, a, "AAAA-AAAA-AAAA"); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

func TestPairingIsRefusedWithoutAnActiveCode(t *testing.T) {
	a := startAgent(t, nil)
	if _, status := pair(t, a, "AAAA-AAAA-AAAA"); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

func TestWebSocketWithoutAValidKeyIsRefused(t *testing.T) {
	a := startAgent(t, nil)
	ha := &homeAssistant{agent: a, fingerprint: fetchFingerprint(t, a.address), key: make([]byte, 32)}
	for name, header := range map[string]string{
		"unknown key": "Bearer " + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"no key":      "Basic x",
	} {
		_, resp, err := ha.dial(t, http.Header{"Authorization": {header}})
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: err = %v, want 401", name, err)
		}
	}
}

func TestBrowserOriginIsRejected(t *testing.T) {
	a := startAgent(t, nil)
	ha, _ := pair(t, a, a.newCode(t))
	_, resp, err := ha.dial(t, http.Header{"Origin": {"https://" + a.address}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want 403", err)
	}
	// The pairing endpoint too.
	req, _ := http.NewRequest(http.MethodPost, "https://"+a.address+"/v1/pair", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://example.com")
	resp, err = pinnedClient(ha.fingerprint).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("pair with Origin: status = %d, want 403", resp.StatusCode)
	}
}

func TestOtherPathsAreNotAnswered(t *testing.T) {
	a := startAgent(t, nil)
	resp, err := pinnedClient(fetchFingerprint(t, a.address)).Get("https://" + a.address + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSourceOutsideTheAllowedAddressesIsDropped(t *testing.T) {
	a := startAgent(t, func(s *Server) {
		s.Config.AllowedSources = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	if conn, err := tls.Dial("tcp", a.address, &tls.Config{InsecureSkipVerify: true}); err == nil {
		conn.Close()
		t.Fatal("TLS handshake worked from a source that is not allowed")
	}
}

func TestOldTLSIsRefused(t *testing.T) {
	a := startAgent(t, nil)
	if conn, err := tls.Dial("tcp", a.address, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}); err == nil {
		conn.Close()
		t.Fatal("TLS 1.2 handshake worked")
	}
}

// open makes a TCP connection that has not logged in.
func open(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// closedByAgent says whether the Agent closed conn within wait.
func closedByAgent(conn net.Conn, wait time.Duration) bool {
	conn.SetReadDeadline(time.Now().Add(wait))
	_, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	return err != nil && !(errors.As(err, &netErr) && netErr.Timeout())
}

func TestConnectionsWithoutAKeyAreCapped(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.MaxUnauthenticatedPerSource = 2 })
	first, second := open(t, a.address), open(t, a.address)
	third := open(t, a.address)
	if !closedByAgent(third, time.Second) {
		t.Fatal("a third connection from one source was kept")
	}
	if closedByAgent(first, 100*time.Millisecond) || closedByAgent(second, 100*time.Millisecond) {
		t.Fatal("the first two connections were closed")
	}
	first.Close()
	time.Sleep(100 * time.Millisecond)
	if closedByAgent(open(t, a.address), 100*time.Millisecond) {
		t.Fatal("a slot was not freed when a connection closed")
	}
}

func TestLoggedInConnectionsDoNotCountAgainstTheCap(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.MaxUnauthenticatedPerSource = 2 })
	ha, _ := pair(t, a, a.newCode(t))
	ha.connect(t)
	time.Sleep(100 * time.Millisecond) // the Agent sees the pairing connections close
	open(t, a.address)
	if closedByAgent(open(t, a.address), 200*time.Millisecond) {
		t.Fatal("a logged-in connection still counts as not logged in")
	}
}

func TestConnectionWithoutAKeyTimesOut(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.UnauthenticatedTimeout = 200 * time.Millisecond })
	if !closedByAgent(open(t, a.address), 2*time.Second) {
		t.Fatal("an idle connection without a key was kept")
	}
}

func TestLoggedInConnectionHasNoTimeout(t *testing.T) {
	a := startAgent(t, func(s *Server) { s.UnauthenticatedTimeout = 200 * time.Millisecond })
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t)
	time.Sleep(400 * time.Millisecond)
	a.server.State.Set(protocol.Groups{System: &protocol.System{CPUPercent: ptr(1.0)}})
	if _, err := read(t, conn, 2*time.Second); err != nil {
		t.Fatalf("logged-in connection closed: %v", err)
	}
}

func TestUnknownRequestGetsUnsupported(t *testing.T) {
	a := startAgent(t, nil)
	ha, _ := pair(t, a, a.newCode(t))
	conn, _, _ := ha.connect(t)
	sendFrame(t, conn, []byte(`{"type":"future_thing","id":"q1","kind":"request"}`))
	message, err := read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reply, ok := message.(*protocol.Unsupported); !ok || reply.ReplyTo != "q1" || reply.RequestType != "future_thing" {
		t.Fatalf("got %+v, want unsupported for q1", message)
	}
}

func TestNoCommonProtocolMajorClosesTheConnection(t *testing.T) {
	a := startAgent(t, nil)
	ha, _ := pair(t, a, a.newCode(t))
	ws, _, err := ha.dial(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := listen(t, ws)
	message, _ := read(t, conn, 5*time.Second)
	hello := message.(*protocol.HelloRequest)
	reply, _ := protocol.Encode(&protocol.HelloReply{ID: "r1", ReplyTo: hello.ID, IntegrationVersion: "9", ProtocolVersion: "9.0", ProtocolMajors: []int{9}})
	sendFrame(t, conn, reply)
	if message, err := read(t, conn, 5*time.Second); err == nil {
		t.Fatalf("got %T, want the connection closed", message)
	}
}

func TestLogsHoldNoKeyOrCode(t *testing.T) {
	a := startAgent(t, nil)
	a.newCode(t)
	pair(t, a, "AAAA-AAAA-AAAA")
	code := a.newCode(t)
	ha, _ := pair(t, a, code)
	ha.connect(t)
	log := a.log.String()
	if !strings.Contains(log, "paired") {
		t.Fatalf("log has no pairing line; is it written? %q", log)
	}
	for _, secret := range []string{
		code, strings.ReplaceAll(code, "-", ""),
		base64.StdEncoding.EncodeToString(ha.key), hex.EncodeToString(ha.key), fmt.Sprintf("%v", ha.key),
	} {
		if strings.Contains(log, secret) {
			t.Errorf("log holds a secret: %q", secret)
		}
	}
}

func TestSnapshotHasEveryGroupAndDeltaOnlyTheChangedOne(t *testing.T) {
	a := startAgent(t, func(s *Server) {
		s.State = NewState(protocol.Groups{
			Agent:  &protocol.AgentInfo{Hostname: "test-host", AgentVersion: "0.0.0", Capabilities: []string{"disks"}, EnabledActions: []protocol.Action{}},
			System: &protocol.System{CPUPercent: ptr(5.0)},
			Disks:  &protocol.Disks{Mounts: []protocol.Mount{{Mount: "/", UsedPercent: ptr(50.0)}}},
			Flags:  &protocol.Flags{RebootRequired: "no", LastBoot: ptr("2026-09-21T14:13:20Z")},
		})
	})
	ha, _ := pair(t, a, a.newCode(t))
	a.server.State.Set(protocol.Groups{Agent: &protocol.AgentInfo{Hostname: "renamed-host", AgentVersion: "0.0.0", Capabilities: []string{"disks"}, EnabledActions: []protocol.Action{}}})
	conn, hello, snapshot := ha.connect(t)
	if hello.Hostname != "renamed-host" || snapshot.Groups.Agent.Hostname != "renamed-host" {
		t.Errorf("hello hostname = %q, want the current one", hello.Hostname)
	}
	if snapshot.Groups.Disks == nil || snapshot.Groups.Flags.RebootRequired != "no" || snapshot.Groups.UpdateRun == nil {
		t.Errorf("snapshot = %+v", snapshot.Groups)
	}

	a.server.State.Set(protocol.Groups{
		System: &protocol.System{CPUPercent: ptr(5.0)},
		Flags:  &protocol.Flags{RebootRequired: "yes", LastBoot: ptr("2026-09-21T14:13:20Z")},
	})
	message, err := read(t, conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	delta := message.(*protocol.Delta)
	if delta.Groups.Flags == nil || delta.Groups.Flags.RebootRequired != "yes" || delta.Groups.System != nil || delta.Groups.Disks != nil {
		t.Errorf("delta = %+v, want only the flags group", delta.Groups)
	}
}
