package pairing

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var start = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// host is one Host's state directory with a fake clock.
type host struct {
	dir      string
	now      time.Time
	pairings *Pairings
}

func newHost(t *testing.T) *host {
	t.Helper()
	h := &host{dir: t.TempDir(), now: start}
	h.pairings = Open(h.dir, func() time.Time { return h.now })
	return h
}

func (h *host) newCode(t *testing.T) string {
	t.Helper()
	code, _, err := NewCode(h.dir, h.now)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

var fingerprint = bytes.Repeat([]byte{7}, 32)

// pair plays Home Assistant: it sends a proof made from code and checks the
// Agent's answer.
func (h *host) pair(t *testing.T, code string) ([]byte, error) {
	t.Helper()
	nonce := make([]byte, 32)
	rand.Read(nonce)
	codeKey := CodeKey(code, nonce)
	key, agentProof, err := h.pairings.Pair("Home", fingerprint, nonce, HomeAssistantProof(codeKey, fingerprint, nonce))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(agentProof, AgentProof(codeKey, fingerprint, nonce, key)) {
		t.Fatal("the Agent's proof does not match the code")
	}
	return key, nil
}

func TestCodeLooksRight(t *testing.T) {
	h := newHost(t)
	code, expires, err := NewCode(h.dir, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}$`).MatchString(code) {
		t.Errorf("code %q is not 3 groups of 4 characters without look-alikes", code)
	}
	if !expires.Equal(start.Add(10 * time.Minute)) {
		t.Errorf("code expires at %v, want 10 minutes after it was made", expires)
	}
}

func TestPairingWithTheCodeGivesAKeyThatLogsIn(t *testing.T) {
	h := newHost(t)
	key, err := h.pair(t, h.newCode(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("key has %d bytes, want 32", len(key))
	}
	if p, ok, _ := h.pairings.Login(key); !ok || p.Name != "Home" {
		t.Fatalf("Login = %+v, %v; want Home, true", p, ok)
	}
}

func TestCodeIsReadWithoutDashesOrCase(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	loose := " " + string(bytes.ToLower([]byte(code[:4]))) + code[5:9] + " " + code[10:]
	if _, err := h.pair(t, loose); err != nil {
		t.Fatalf("code typed as %q: %v", loose, err)
	}
}

func TestWrongCodeIsRefused(t *testing.T) {
	h := newHost(t)
	h.newCode(t)
	if _, err := h.pair(t, "AAAA-AAAA-AAAA"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestExpiredCodeIsRefused(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	h.now = start.Add(10*time.Minute + time.Second)
	if _, err := h.pair(t, code); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestCodeWorksOnce(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	if _, err := h.pair(t, code); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pair(t, code); !errors.Is(err, ErrRefused) {
		t.Fatalf("second use: err = %v, want ErrRefused", err)
	}
}

func TestSixthTryIsRefusedEvenWithTheRightCode(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	for range 5 {
		if _, err := h.pair(t, "AAAA-AAAA-AAAA"); !errors.Is(err, ErrRefused) {
			t.Fatalf("wrong code: err = %v, want ErrRefused", err)
		}
	}
	if _, err := h.pair(t, code); !errors.Is(err, ErrRefused) {
		t.Fatalf("6th try: err = %v, want ErrRefused", err)
	}
}

func TestFifthTryStillWorks(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	for range 4 {
		h.pair(t, "AAAA-AAAA-AAAA")
	}
	if _, err := h.pair(t, code); err != nil {
		t.Fatalf("5th try: %v", err)
	}
}

func TestNoActiveCodeRefusesPairing(t *testing.T) {
	h := newHost(t)
	if _, err := h.pair(t, "AAAA-AAAA-AAAA"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestProofForAnotherCertificateIsRefused(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	nonce := make([]byte, 32)
	other := bytes.Repeat([]byte{8}, 32)
	proof := HomeAssistantProof(CodeKey(code, nonce), other, nonce)
	if _, _, err := h.pairings.Pair("Home", fingerprint, nonce, proof); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestUnconfirmedKeyIsDroppedWhenTheCodeExpires(t *testing.T) {
	h := newHost(t)
	key, err := h.pair(t, h.newCode(t))
	if err != nil {
		t.Fatal(err)
	}
	h.now = start.Add(10*time.Minute + time.Second)
	if _, ok, _ := h.pairings.Login(key); ok {
		t.Fatal("an unconfirmed key logged in after the code expired")
	}
}

func TestConfirmedKeyStillLogsInAfterRestart(t *testing.T) {
	h := newHost(t)
	key, _ := h.pair(t, h.newCode(t))
	if _, ok, _ := h.pairings.Login(key); !ok {
		t.Fatal("first login failed")
	}
	h.now = start.Add(24 * time.Hour)
	restarted := Open(h.dir, func() time.Time { return h.now })
	if _, ok, _ := restarted.Login(key); !ok {
		t.Fatal("a confirmed key did not log in after a restart")
	}
}

func TestUnknownKeyDoesNotLogIn(t *testing.T) {
	h := newHost(t)
	h.pair(t, h.newCode(t))
	if _, ok, _ := h.pairings.Login(make([]byte, 32)); ok {
		t.Fatal("an unknown key logged in")
	}
}

func TestHostStoresNoKeyAndNoCodeAfterPairing(t *testing.T) {
	h := newHost(t)
	code := h.newCode(t)
	key, _ := h.pair(t, code)
	h.pairings.Login(key)
	files, _ := filepath.Glob(filepath.Join(h.dir, "*"))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, key) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(key))) ||
			bytes.Contains(data, []byte(hex.EncodeToString(key))) || bytes.Contains(data, []byte(code)) {
			t.Errorf("%s holds the key or the code", filepath.Base(file))
		}
	}
}

// The shared vector in protocol/pairing_vector.json keeps the Agent and the
// Integration in step.
func TestPairingVector(t *testing.T) {
	data, err := os.ReadFile("../../../protocol/pairing_vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	b := func(name string) []byte {
		value, err := hex.DecodeString(v[name])
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	codeKey := CodeKey(v["code"], b("nonce"))
	if !bytes.Equal(codeKey, b("code_key")) {
		t.Error("code key does not match the vector")
	}
	if !bytes.Equal(HomeAssistantProof(codeKey, b("fingerprint"), b("nonce")), b("home_assistant_proof")) {
		t.Error("Home Assistant proof does not match the vector")
	}
	if !bytes.Equal(AgentProof(codeKey, b("fingerprint"), b("nonce"), b("key")), b("agent_proof")) {
		t.Error("Agent proof does not match the vector")
	}
	if got := ID(b("key")); got != v["pairing_id"] {
		t.Errorf("ID = %s, want %s", got, v["pairing_id"])
	}
}

// login pairs with a new code and confirms the key, as Home Assistant does.
func (h *host) login(t *testing.T, name string) []byte {
	t.Helper()
	nonce := make([]byte, 32)
	rand.Read(nonce)
	code := h.newCode(t)
	key, _, err := h.pairings.Pair(name, fingerprint, nonce, HomeAssistantProof(CodeKey(code, nonce), fingerprint, nonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.pairings.Login(key); !ok || err != nil {
		t.Fatalf("Login = %v, %v", ok, err)
	}
	return key
}

func TestListShowsEachPairingWithNameCreatedAndLastSeen(t *testing.T) {
	h := newHost(t)
	home := h.login(t, "Home")
	h.now = start.Add(time.Hour)
	h.login(t, "Office")
	h.now = start.Add(2 * time.Hour)
	h.pairings.Login(home)

	list, err := h.pairings.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List has %d Pairings, want 2", len(list))
	}
	if list[0].Name != "Home" || list[0].ID != ID(home) || !list[0].Created.Equal(start) || !list[0].LastSeen.Equal(start.Add(2*time.Hour)) {
		t.Errorf("first = %+v", list[0])
	}
	if list[1].Name != "Office" || !list[1].Created.Equal(start.Add(time.Hour)) || !list[1].LastSeen.Equal(start.Add(time.Hour)) {
		t.Errorf("second = %+v", list[1])
	}
}

func TestIDIsEightHexCharacters(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(ID(make([]byte, 32))) {
		t.Errorf("ID = %q", ID(make([]byte, 32)))
	}
}

func TestTwoPairingsWorkIndependently(t *testing.T) {
	h := newHost(t)
	home := h.login(t, "Home")
	office := h.login(t, "Office")

	removed, err := h.pairings.Remove(ID(home))
	if err != nil || removed.Name != "Home" {
		t.Fatalf("Remove = %+v, %v", removed, err)
	}
	if _, ok, _ := h.pairings.Login(home); ok {
		t.Error("a removed key still logs in")
	}
	if p, ok, _ := h.pairings.Login(office); !ok || p.Name != "Office" {
		t.Errorf("the other Pairing stopped working: %+v, %v", p, ok)
	}
}

func TestRemoveByUniqueName(t *testing.T) {
	h := newHost(t)
	h.login(t, "Home")
	office := h.login(t, "Office")
	if _, err := h.pairings.Remove("Home"); err != nil {
		t.Fatal(err)
	}
	list, _ := h.pairings.List()
	if len(list) != 1 || list[0].ID != ID(office) {
		t.Errorf("List = %+v, want only Office", list)
	}
}

func TestRemoveRefusesAnAmbiguousOrUnknownName(t *testing.T) {
	h := newHost(t)
	h.login(t, "Home")
	h.login(t, "Home")
	if _, err := h.pairings.Remove("Home"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous", err)
	}
	if _, err := h.pairings.Remove("nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if list, _ := h.pairings.List(); len(list) != 2 {
		t.Errorf("List has %d Pairings, want 2 (nothing removed)", len(list))
	}
}

// A root owner command and the network part are two processes. A login in
// one must not bring back a Pairing the other removed.
func TestLoginInAnotherProcessDoesNotBringBackARemovedPairing(t *testing.T) {
	h := newHost(t)
	home := h.login(t, "Home")
	owner := Open(h.dir, func() time.Time { return h.now })
	if _, err := owner.Remove(ID(home)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.pairings.Login(home); ok {
		t.Fatal("a key removed by another process still logs in")
	}
	if list, _ := owner.List(); len(list) != 0 {
		t.Errorf("List = %+v, want none", list)
	}
}

func TestStaleListsPairingsNotSeenFor90Days(t *testing.T) {
	h := newHost(t)
	h.login(t, "Old")
	h.now = start.Add(89 * 24 * time.Hour)
	h.login(t, "Recent")
	h.now = start.Add(90 * 24 * time.Hour)

	list, _ := h.pairings.List()
	stale := Stale(list, h.now)
	if len(stale) != 1 || stale[0].Name != "Old" {
		t.Errorf("Stale = %+v, want only Old", stale)
	}
}
