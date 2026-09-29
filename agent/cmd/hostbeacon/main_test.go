package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

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
