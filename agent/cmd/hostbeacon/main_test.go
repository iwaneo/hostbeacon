package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
)

func TestForbiddenGroups(t *testing.T) {
	if got := forbiddenGroups([]string{"hostbeacon", "docker", "systemd-journal", "adm", "disk"}); !slices.Equal(got, []string{"docker", "adm", "disk"}) {
		t.Errorf("forbiddenGroups = %v", got)
	}
	if got := forbiddenGroups([]string{"hostbeacon"}); len(got) != 0 {
		t.Errorf("forbiddenGroups = %v, want none", got)
	}
}

// paired makes a confirmed Pairing in dir, as Home Assistant would.
func paired(t *testing.T, dir, name string, now time.Time) []byte {
	t.Helper()
	code, _, err := pairing.NewCode(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, nonce := make([]byte, 32), make([]byte, 32)
	pairings := pairing.Open(dir, func() time.Time { return now })
	key, _, err := pairings.Pair(name, fingerprint, nonce, pairing.HomeAssistantProof(pairing.CodeKey(code, nonce), fingerprint, nonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pairings.Login(key); !ok || err != nil {
		t.Fatal("login failed", err)
	}
	return key
}

func TestPairingsListShowsIDNameCreatedLastSeenAndStale(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	old := paired(t, dir, "Old Home", start)
	recent := paired(t, dir, "Office", start.Add(100*24*time.Hour))

	var out bytes.Buffer
	if err := pairings([]string{"list", "--state-dir", dir}, &out, start.Add(101*24*time.Hour), time.UTC); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		pairing.ID(old), "Old Home", "2026-01-02 03:04",
		pairing.ID(recent), "Office", "2026-04-12 03:04",
		"not seen for 90 days or more",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output has no %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "not seen for 90 days") != 1 {
		t.Errorf("want one stale warning:\n%s", text)
	}
}

func TestPairingsRemoveRemovesOnlyThatPairing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	home := paired(t, dir, "Home", now)
	office := paired(t, dir, "Office", now)

	var out bytes.Buffer
	if err := pairings([]string{"remove", "--state-dir", dir, pairing.ID(home)}, &out, now, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Removed the Pairing Home") {
		t.Errorf("output = %q", out.String())
	}
	list, _ := pairing.Open(dir, time.Now).List()
	if len(list) != 1 || list[0].ID != pairing.ID(office) {
		t.Errorf("Pairings = %+v, want only Office", list)
	}
	if err := pairings([]string{"remove", "--state-dir", dir, "nothing"}, &out, now, time.UTC); err == nil {
		t.Error("removing an unknown Pairing did not fail")
	}
}

func TestPairingsWithoutASubcommandFails(t *testing.T) {
	var out bytes.Buffer
	if err := pairings(nil, &out, time.Now(), time.UTC); err == nil {
		t.Error("no error")
	}
}

var installSignals = identity.Signals{Values: map[string]string{
	identity.SignalMachineID:  "ae2c94ba02ba4c71aebca6465f4a166b",
	identity.SignalSMBIOSUUID: "7b0a52f3-6a4e-4c8e-9a51-3cb4e2d1f001",
}}

// testOwner runs owner commands on a temporary state directory, with the
// install signals, owner input, and a fake Agent restart.
type testOwner struct {
	owner
	stateDir  string
	actionLog string
	output    *bytes.Buffer
	restarts  int
}

func newTestOwner(t *testing.T, input string) *testOwner {
	t.Helper()
	o := &testOwner{stateDir: t.TempDir(), actionLog: t.TempDir(), output: &bytes.Buffer{}}
	o.owner = owner{
		in:      strings.NewReader(input),
		out:     o.output,
		signals: func() identity.Signals { return installSignals },
		restart: func() error { o.restarts++; return nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("not on this Host")
		},
		tailscale: func() bool { return false },
	}
	if _, _, err := identity.Start(o.stateDir, installSignals); err != nil {
		t.Fatal(err)
	}
	return o
}

func (o *testOwner) args(more ...string) []string {
	return append([]string{"--state-dir", o.stateDir, "--action-log", o.actionLog}, more...)
}

func TestReadSignals(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	uuid := "7b0a52f3-6a4e-4c8e-9a51-3cb4e2d1f001"
	smbios := func(value *string, err error) func(context.Context) (*string, error) {
		return func(context.Context) (*string, error) { return value, err }
	}

	os.WriteFile(filepath.Join(root, "etc", "machine-id"), []byte("AE2C94BA02BA4C71AEBCA6465F4A166B\n"), 0o444)
	got := readSignals(context.Background(), root, smbios(&uuid, nil))
	if got.Values[identity.SignalMachineID] != "ae2c94ba02ba4c71aebca6465f4a166b" || got.Values[identity.SignalSMBIOSUUID] != uuid || len(got.Unreadable) != 0 {
		t.Errorf("both readable: %+v", got)
	}

	// A Host without SMBIOS (a Raspberry Pi) and without a machine ID yet.
	os.Remove(filepath.Join(root, "etc", "machine-id"))
	os.WriteFile(filepath.Join(root, "etc", "machine-id"), []byte("uninitialized\n"), 0o444)
	got = readSignals(context.Background(), root, smbios(nil, nil))
	if len(got.Values) != 0 || len(got.Unreadable) != 0 {
		t.Errorf("both missing: %+v", got)
	}

	os.Remove(filepath.Join(root, "etc", "machine-id"))
	got = readSignals(context.Background(), root, smbios(nil, errors.New("helper: connection refused")))
	if !slices.Equal(got.Unreadable, []string{identity.SignalSMBIOSUUID}) || len(got.Values) != 0 {
		t.Errorf("helper down: %+v", got)
	}

	if os.Geteuid() != 0 {
		os.WriteFile(filepath.Join(root, "etc", "machine-id"), []byte("ae2c94ba02ba4c71aebca6465f4a166b\n"), 0o000)
		got = readSignals(context.Background(), root, smbios(&uuid, nil))
		if !slices.Equal(got.Unreadable, []string{identity.SignalMachineID}) {
			t.Errorf("machine ID not readable: %+v", got)
		}
	}
}

func TestResetIdentityAsksFirst(t *testing.T) {
	o := newTestOwner(t, "no\n")
	before, _ := identity.ReadStatus(o.stateDir)
	paired(t, o.stateDir, "Home", time.Now())

	if err := resetIdentity(o.args(), o.owner); err == nil {
		t.Error("no error when the owner did not say yes")
	}
	after, _ := identity.ReadStatus(o.stateDir)
	list, _ := pairing.Open(o.stateDir, time.Now).List()
	if after.InstanceID != before.InstanceID || len(list) != 1 || o.restarts != 0 {
		t.Error("reset-identity changed something without a yes")
	}
	if !strings.Contains(o.output.String(), "best effort") {
		t.Errorf("reset-identity does not say that clone detection is best effort:\n%s", o.output)
	}
}

func TestResetIdentityMakesANewIdentityAndLogsIt(t *testing.T) {
	o := newTestOwner(t, "yes\n")
	before, _ := identity.ReadStatus(o.stateDir)
	paired(t, o.stateDir, "Home", time.Now())

	if err := resetIdentity(o.args(), o.owner); err != nil {
		t.Fatal(err)
	}
	after, _ := identity.ReadStatus(o.stateDir)
	list, _ := pairing.Open(o.stateDir, time.Now).List()
	if after.InstanceID == before.InstanceID || !slices.Equal(after.CopiedFrom, []string{before.InstanceID}) || len(list) != 0 {
		t.Errorf("after reset-identity: %+v, %d Pairings", after, len(list))
	}
	if o.restarts != 1 {
		t.Errorf("restarts = %d, want 1", o.restarts)
	}
	logs, _ := filepath.Glob(filepath.Join(o.actionLog, "actions-*.log"))
	if len(logs) != 1 {
		t.Fatalf("Action log files = %v", logs)
	}
	data, _ := os.ReadFile(logs[0])
	if !strings.Contains(string(data), `"identity_copy"`) || !strings.Contains(string(data), after.InstanceID) {
		t.Errorf("Action log = %s", data)
	}
}

func TestKeepIdentityEndsTheHold(t *testing.T) {
	o := newTestOwner(t, "")
	unreadable := identity.Signals{Values: map[string]string{identity.SignalMachineID: installSignals.Values[identity.SignalMachineID]}, Unreadable: []string{identity.SignalSMBIOSUUID}}
	identity.Start(o.stateDir, unreadable)
	o.signals = func() identity.Signals { return unreadable }

	if err := keepIdentity(o.args(), o.owner); err == nil || !strings.Contains(err.Error(), "--drop-missing") {
		t.Errorf("keep-identity with an unreadable signal: %v, want a hint to --drop-missing", err)
	}
	if err := keepIdentity(o.args("--drop-missing"), o.owner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o.output.String(), "Warning") || !strings.Contains(o.output.String(), "SMBIOS UUID") {
		t.Errorf("no warning about the dropped signal:\n%s", o.output)
	}
	if status, _ := identity.ReadStatus(o.stateDir); len(status.Hold) != 0 || o.restarts != 1 {
		t.Errorf("hold = %v, restarts = %d", status.Hold, o.restarts)
	}
}

func TestRegenerateKeyListsThePairingsFirst(t *testing.T) {
	o := newTestOwner(t, "yes\n")
	paired(t, o.stateDir, "Office", time.Now())

	if err := regenerateKey(o.args(), o.owner); err != nil {
		t.Fatal(err)
	}
	text := o.output.String()
	if i, j := strings.Index(text, "Office"), strings.Index(text, "Removed"); i < 0 || j < i {
		t.Errorf("output does not list the Pairing before removing it:\n%s", text)
	}
	list, _ := pairing.Open(o.stateDir, time.Now).List()
	if len(list) != 0 || o.restarts != 1 {
		t.Errorf("%d Pairings, %d restarts", len(list), o.restarts)
	}
}

func TestStatusSaysWhatToRunInIdentityHold(t *testing.T) {
	o := newTestOwner(t, "")
	var out bytes.Buffer
	o.out = &out
	if err := status(o.args(), o.owner); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hold") {
		t.Errorf("status shows a hold without one:\n%s", out.String())
	}

	identity.Start(o.stateDir, identity.Signals{Values: map[string]string{identity.SignalMachineID: installSignals.Values[identity.SignalMachineID]}})
	out.Reset()
	if err := status(o.args(), o.owner); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"identity hold", "SMBIOS UUID", "sudo hostbeacon keep-identity", "sudo hostbeacon reset-identity"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status has no %q:\n%s", want, out.String())
		}
	}
}
