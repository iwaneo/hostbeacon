package identity

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
)

var install = Signals{Values: map[string]string{
	SignalMachineID:  "ae2c94ba02ba4c71aebca6465f4a166b",
	SignalSMBIOSUUID: "7b0a52f3-6a4e-4c8e-9a51-3cb4e2d1f001",
}}

// with returns the install signals with one signal changed, or unreadable
// when value is "".
func with(name, value string) Signals {
	s := Signals{Values: map[string]string{}}
	for k, v := range install.Values {
		s.Values[k] = v
	}
	if value == "" {
		delete(s.Values, name)
		s.Unreadable = []string{name}
	} else {
		s.Values[name] = value
	}
	return s
}

func start(t *testing.T, dir string, signals Signals) (Identity, Check) {
	t.Helper()
	id, check, err := Start(dir, signals)
	if err != nil {
		t.Fatal(err)
	}
	return id, check
}

// pair makes a confirmed Pairing in dir, as Home Assistant would.
func pair(t *testing.T, dir string) {
	t.Helper()
	code, _, err := pairing.NewCode(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, nonce := make([]byte, 32), make([]byte, 32)
	pairings := pairing.Open(dir, time.Now)
	key, _, err := pairings.Pair("Home", fingerprint, nonce, pairing.HomeAssistantProof(pairing.CodeKey(code, nonce), fingerprint, nonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pairings.Login(key); !ok || err != nil {
		t.Fatal("login failed", err)
	}
}

func pairingCount(t *testing.T, dir string) int {
	t.Helper()
	list, err := pairing.Open(dir, time.Now).List()
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestFirstStartMakesIdentityAndLaterStartsKeepIt(t *testing.T) {
	dir := t.TempDir()
	first, check := start(t, dir, install)
	if check.Outcome != Same {
		t.Errorf("first start outcome = %v, want Same", check.Outcome)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first.InstanceID) {
		t.Errorf("instance ID %q is not a UUID v4", first.InstanceID)
	}
	if first.Fingerprint != sha256.Sum256(first.Certificate.Certificate[0]) {
		t.Error("fingerprint is not the SHA-256 of the certificate")
	}
	info, err := os.Stat(filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	for _, raw := range install.Values {
		if regexp.MustCompile(raw).Match(data) {
			t.Errorf("identity.json holds the raw signal %s, not a hash", raw)
		}
	}

	again, check := start(t, dir, install)
	if check.Outcome != Same || again.InstanceID != first.InstanceID || again.Fingerprint != first.Fingerprint {
		t.Error("a later start changed the instance ID or the certificate")
	}
	if len(again.CopiedFrom) != 0 {
		t.Errorf("copied-from = %v, want none", again.CopiedFrom)
	}
}

func TestChangedSignalMakesANewIdentityWithoutPairings(t *testing.T) {
	for _, name := range []string{SignalMachineID, SignalSMBIOSUUID} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			original, _ := start(t, dir, install)
			hostID := NewUUID()
			if err := AddKnownID(dir, hostID); err != nil {
				t.Fatal(err)
			}
			pair(t, dir)
			if _, _, err := pairing.NewCode(dir, time.Now()); err != nil {
				t.Fatal(err)
			}

			changed := with(name, "0000000000000000000000000000000f")
			copied, check := start(t, dir, changed)
			if check.Outcome != Copied {
				t.Fatalf("outcome = %v, want Copied", check.Outcome)
			}
			if copied.InstanceID == original.InstanceID || copied.Fingerprint == original.Fingerprint {
				t.Error("the copy kept the instance ID or the certificate")
			}
			slices.Sort(copied.CopiedFrom)
			want := []string{original.InstanceID, hostID}
			slices.Sort(want)
			if !slices.Equal(copied.CopiedFrom, want) {
				t.Errorf("copied-from = %v, want %v", copied.CopiedFrom, want)
			}
			if n := pairingCount(t, dir); n != 0 {
				t.Errorf("%d Pairings left, want none", n)
			}
			if _, err := os.Stat(filepath.Join(dir, "pairing-code.json")); !os.IsNotExist(err) {
				t.Error("the Pairing code is still there")
			}

			// The copy is now itself: the next start keeps it.
			again, check := start(t, dir, changed)
			if check.Outcome != Same || again.InstanceID != copied.InstanceID || again.Fingerprint != copied.Fingerprint {
				t.Errorf("the next start did not keep the new identity: %v", check.Outcome)
			}

			// A copy of the copy remembers every earlier ID.
			third, _ := start(t, dir, with(name, "0000000000000000000000000000000e"))
			if !slices.Contains(third.CopiedFrom, original.InstanceID) || !slices.Contains(third.CopiedFrom, copied.InstanceID) {
				t.Errorf("copied-from = %v, want both earlier instance IDs", third.CopiedFrom)
			}
		})
	}
}

func TestUnreadableSignalHoldsTheIdentity(t *testing.T) {
	for name, signals := range map[string]Signals{
		"unreadable": with(SignalSMBIOSUUID, ""),
		"gone":       {Values: map[string]string{SignalMachineID: install.Values[SignalMachineID]}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			original, _ := start(t, dir, install)
			pair(t, dir)

			held, check := start(t, dir, signals)
			if check.Outcome != Hold || !slices.Equal(check.Missing, []string{SignalSMBIOSUUID}) {
				t.Fatalf("check = %+v, want Hold for smbios_uuid", check)
			}
			if held.InstanceID != original.InstanceID {
				t.Error("identity hold changed the instance ID")
			}
			if pairingCount(t, dir) != 1 {
				t.Error("identity hold removed the Pairings")
			}
			if status, err := ReadStatus(dir); err != nil || !slices.Equal(status.Hold, []string{SignalSMBIOSUUID}) {
				t.Errorf("status = %+v, %v; want the hold recorded", status, err)
			}

			// The next start checks again.
			again, check := start(t, dir, install)
			if check.Outcome != Same || again.Fingerprint != original.Fingerprint {
				t.Errorf("outcome = %v, want Same with the old certificate", check.Outcome)
			}
			if status, _ := ReadStatus(dir); len(status.Hold) != 0 {
				t.Errorf("hold = %v after a good start, want none", status.Hold)
			}
		})
	}
}

func TestChangedWinsOverUnreadable(t *testing.T) {
	dir := t.TempDir()
	start(t, dir, install)
	signals := with(SignalSMBIOSUUID, "")
	signals.Values[SignalMachineID] = "0000000000000000000000000000000f"
	if _, check := start(t, dir, signals); check.Outcome != Copied {
		t.Errorf("outcome = %v, want Copied", check.Outcome)
	}
}

func TestSignalMissingAtInstallIsNotChecked(t *testing.T) {
	dir := t.TempDir()
	lxc := Signals{Values: map[string]string{SignalMachineID: install.Values[SignalMachineID]}}
	start(t, dir, lxc)
	if _, check := start(t, dir, install); check.Outcome != Same {
		t.Errorf("outcome = %v, want Same", check.Outcome)
	}
}

func TestResetDoesTheCopyPathByHand(t *testing.T) {
	dir := t.TempDir()
	original, _ := start(t, dir, install)
	pair(t, dir)

	reset, err := Reset(dir, install)
	if err != nil {
		t.Fatal(err)
	}
	if reset.InstanceID == original.InstanceID || reset.Fingerprint == original.Fingerprint {
		t.Error("reset kept the instance ID or the certificate")
	}
	if !slices.Equal(reset.CopiedFrom, []string{original.InstanceID}) {
		t.Errorf("copied-from = %v", reset.CopiedFrom)
	}
	if pairingCount(t, dir) != 0 {
		t.Error("reset kept the Pairings")
	}
	again, check := start(t, dir, install)
	if check.Outcome != Same || again.InstanceID != reset.InstanceID {
		t.Errorf("the start after reset: %v, %s", check.Outcome, again.InstanceID)
	}
}

func TestKeepStoresTheCurrentFingerprint(t *testing.T) {
	dir := t.TempDir()
	original, _ := start(t, dir, install)
	pair(t, dir)
	moved := with(SignalSMBIOSUUID, "11111111-2222-4333-8444-555555555555")

	if _, err := Keep(dir, moved, false); err != nil {
		t.Fatal(err)
	}
	kept, check := start(t, dir, moved)
	if check.Outcome != Same || kept.InstanceID != original.InstanceID || pairingCount(t, dir) != 1 {
		t.Errorf("after keep-identity: %v, Pairings %d", check.Outcome, pairingCount(t, dir))
	}
}

func TestKeepNeedsEverySignalUnlessDropMissing(t *testing.T) {
	dir := t.TempDir()
	original, _ := start(t, dir, install)
	unreadable := with(SignalSMBIOSUUID, "")
	if _, check := start(t, dir, unreadable); check.Outcome != Hold {
		t.Fatalf("outcome = %v, want Hold", check.Outcome)
	}

	if _, err := Keep(dir, unreadable, false); err == nil {
		t.Fatal("keep-identity with an unreadable signal did not fail")
	}
	if status, _ := ReadStatus(dir); len(status.Hold) == 0 {
		t.Error("a failed keep-identity ended the hold")
	}

	dropped, err := Keep(dir, unreadable, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dropped, []string{SignalSMBIOSUUID}) {
		t.Errorf("dropped = %v", dropped)
	}
	if status, _ := ReadStatus(dir); len(status.Hold) != 0 {
		t.Errorf("hold = %v after keep-identity, want none", status.Hold)
	}
	kept, check := start(t, dir, unreadable)
	if check.Outcome != Same || kept.InstanceID != original.InstanceID {
		t.Errorf("outcome = %v after --drop-missing, want Same", check.Outcome)
	}
}

func TestRegenerateKeyMakesANewCertificateAndRemovesEveryPairing(t *testing.T) {
	dir := t.TempDir()
	original, _ := start(t, dir, install)
	pair(t, dir)
	pair(t, dir)

	regenerated, err := RegenerateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if regenerated.InstanceID != original.InstanceID {
		t.Error("regenerate-key changed the instance ID")
	}
	if regenerated.Fingerprint == original.Fingerprint {
		t.Error("regenerate-key kept the certificate")
	}
	if pairingCount(t, dir) != 0 {
		t.Error("regenerate-key kept Pairings")
	}
	again, _ := start(t, dir, install)
	if again.Fingerprint != regenerated.Fingerprint {
		t.Error("the next start did not use the new certificate")
	}
}

func TestAddKnownIDKeepsEachIDOnce(t *testing.T) {
	dir := t.TempDir()
	original, _ := start(t, dir, install)
	for range 2 {
		if err := AddKnownID(dir, original.InstanceID); err != nil {
			t.Fatal(err)
		}
	}
	copied, _ := start(t, dir, with(SignalMachineID, "0000000000000000000000000000000f"))
	if !slices.Equal(copied.CopiedFrom, []string{original.InstanceID}) {
		t.Errorf("copied-from = %v, want the one ID", copied.CopiedFrom)
	}
}

func TestNewUUIDIsRandom(t *testing.T) {
	if NewUUID() == NewUUID() {
		t.Error("two UUIDs are the same")
	}
}
