package main

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// setupHost is a Host for setup: a running firewall (or none), Tailscale
// (or not), and the commands setup ran.
type setupHost struct {
	*testOwner
	configPath string
	firewall   string
	commands   []string
}

func newSetupHost(t *testing.T, input, firewall string, tailscale bool) *setupHost {
	t.Helper()
	h := &setupHost{testOwner: newTestOwner(t, input), configPath: filepath.Join(t.TempDir(), "config.json"), firewall: firewall}
	if _, err := config.WriteDefault(h.configPath); err != nil {
		t.Fatal(err)
	}
	h.tailscale = func() bool { return tailscale }
	h.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		h.commands = append(h.commands, line)
		switch {
		case line == "firewall-cmd --state" && h.firewall == "firewalld":
			return []byte("running\n"), nil
		case line == "ufw status" && h.firewall == "ufw":
			return []byte("Status: active\n"), nil
		case line == "ufw status":
			return []byte("Status: inactive\n"), nil
		case strings.HasPrefix(line, "firewall-cmd") && h.firewall != "firewalld":
			return nil, errors.New("not running")
		}
		return nil, nil
	}
	return h
}

func (h *setupHost) setup(t *testing.T, flags ...string) error {
	t.Helper()
	return setup(append([]string{"--config", h.configPath, "--state-dir", h.stateDir}, flags...), h.owner)
}

func (h *setupHost) config(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *setupHost) ran(prefix string) []string {
	var found []string
	for _, line := range h.commands {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	return found
}

func TestSetupDefaultsTurnNothingOnAndOpenNothing(t *testing.T) {
	// Enter for every question: three Actions, the Tailscale address, the
	// firewall, and the Pairing code.
	h := newSetupHost(t, "\n\n\n\n\n\n", "firewalld", true)
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	c := h.config(t)
	if len(c.EnabledActions) != 0 {
		t.Errorf("enabled actions = %v, want none", c.EnabledActions)
	}
	if c.Allows(mustAddr("100.101.102.103")) || c.VPNAddress.IsValid() {
		t.Error("a VPN address is allowed")
	}
	if opened := h.ran("firewall-cmd --permanent"); len(opened) > 0 {
		t.Errorf("the firewall was changed: %v", opened)
	}
	for _, want := range []string{"Turn on Reboot?", "Turn on Update run?", "Turn on Agent update?", "Tailscale", "Open TCP port 8743", "Pairing code: "} {
		if !strings.Contains(h.output.String(), want) {
			t.Errorf("the output has no %q:\n%s", want, h.output.String())
		}
	}
	if h.restarts != 1 {
		t.Errorf("%d restarts, want 1: the Agent reads the Actions at start", h.restarts)
	}
}

func TestSetupTurnsOnWhatTheOwnerSaysYesTo(t *testing.T) {
	h := newSetupHost(t, "y\nyes\nn\n100.64.0.0/10\n100.101.102.103\ny\nn\n", "firewalld", true)
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	c := h.config(t)
	if !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionReboot, protocol.ActionUpdateRun}) {
		t.Errorf("enabled actions = %v, want reboot and update_run", c.EnabledActions)
	}
	if c.VPNAddress != mustAddr("100.101.102.103") || !c.Allows(mustAddr("100.101.102.103")) {
		t.Errorf("VPN address = %v, want 100.101.102.103 allowed", c.VPNAddress)
	}
	if c.Allows(mustAddr("100.101.102.104")) {
		t.Error("another Tailscale address is allowed: setup must allow only Home Assistant's single address")
	}
	want := []string{"firewall-cmd --permanent --add-port=8743/tcp", "firewall-cmd --reload"}
	if got := h.ran("firewall-cmd --"); !slices.Equal(got[1:], want) {
		t.Errorf("firewall commands = %v, want %v after the state check", got, want)
	}
	if strings.Contains(h.output.String(), "Pairing code: ") {
		t.Error("a Pairing code was shown after no")
	}
}

func TestSetupRunAgainKeepsWhatIsOnByDefault(t *testing.T) {
	// Enter keeps each Action as it is: on for Reboot, off for the others.
	h := newSetupHost(t, "\n\n\n\n", "", false)
	if err := config.WriteSetup(h.configPath, config.Setup{EnabledActions: []protocol.Action{protocol.ActionReboot}}); err != nil {
		t.Fatal(err)
	}
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionReboot}) {
		t.Errorf("enabled actions = %v, want only reboot kept", c.EnabledActions)
	}
	if !strings.Contains(h.output.String(), "Reboot is on now") || !strings.Contains(h.output.String(), "Keep it on? [Y/n]") {
		t.Errorf("the Reboot question does not say it is on:\n%s", h.output.String())
	}
}

func TestSetupAsksAboutTailscaleOnlyWhenItIsThere(t *testing.T) {
	h := newSetupHost(t, "\n\n\n\n", "", false)
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.output.String(), "Tailscale runs") {
		t.Errorf("setup asked about Tailscale without it:\n%s", h.output.String())
	}
	if !strings.Contains(h.output.String(), "firewalld or ufw") {
		t.Errorf("setup does not say that no known firewall runs:\n%s", h.output.String())
	}
}

func TestSetupOpensThePortInUfw(t *testing.T) {
	h := newSetupHost(t, "", "ufw", false)
	if err := h.setup(t, "--open-firewall"); err != nil {
		t.Fatal(err)
	}
	if got := h.ran("ufw allow"); !slices.Equal(got, []string{"ufw allow 8743/tcp"}) {
		t.Errorf("ufw commands = %v", got)
	}
}

func TestSetupFlagsFormAsksNothing(t *testing.T) {
	// No input at all: a question would read end of file.
	h := newSetupHost(t, "", "firewalld", true)
	if err := h.setup(t, "--actions", "reboot,agent_update", "--vpn-address", "100.101.102.103", "--open-firewall"); err != nil {
		t.Fatal(err)
	}
	c := h.config(t)
	if !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionReboot, protocol.ActionAgentUpdate}) {
		t.Errorf("enabled actions = %v", c.EnabledActions)
	}
	if !c.Allows(mustAddr("100.101.102.103")) {
		t.Error("the VPN address is refused")
	}
	if len(h.ran("firewall-cmd --permanent --add-port=8743/tcp")) != 1 {
		t.Errorf("the port was not opened: %v", h.commands)
	}
	if strings.Contains(h.output.String(), "?") {
		t.Errorf("the flags form asked a question:\n%s", h.output.String())
	}
	if strings.Contains(h.output.String(), "Pairing code: ") {
		t.Error("a Pairing code was shown without --pair")
	}

	// A second run with only --pair keeps the Actions and the VPN address,
	// and shows a code.
	h.output.Reset()
	if err := h.setup(t, "--pair"); err != nil {
		t.Fatal(err)
	}
	c = h.config(t)
	if len(c.EnabledActions) != 2 || !c.Allows(mustAddr("100.101.102.103")) {
		t.Errorf("enabled actions = %v, VPN address allowed = %v; want both kept", c.EnabledActions, c.Allows(mustAddr("100.101.102.103")))
	}
	if !strings.Contains(h.output.String(), "Pairing code: ") {
		t.Errorf("no Pairing code:\n%s", h.output.String())
	}
}

func TestSetupFlagsFormRefusesBadValues(t *testing.T) {
	for _, flags := range [][]string{
		{"--actions", "reboot,shutdown"},
		{"--vpn-address", "100.64.0.0/10"},
		{"--vpn-address", "tailscale"},
		{"--actions", "reboot", "extra"},
	} {
		h := newSetupHost(t, "", "firewalld", true)
		if err := h.setup(t, flags...); err == nil {
			t.Errorf("%v: no error", flags)
		}
		if c := h.config(t); len(c.EnabledActions) != 0 || len(h.ran("firewall-cmd --permanent")) > 0 || h.restarts > 0 {
			t.Errorf("%v: something changed", flags)
		}
	}
}

func TestSetupFlagsFormNone(t *testing.T) {
	h := newSetupHost(t, "", "", false)
	if err := h.setup(t, "--actions", "reboot", "--vpn-address", "100.101.102.103"); err != nil {
		t.Fatal(err)
	}
	if err := h.setup(t, "--actions", "none", "--vpn-address", "none"); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); len(c.EnabledActions) != 0 || c.VPNAddress.IsValid() {
		t.Errorf("enabled actions = %v, VPN address = %v; want neither", c.EnabledActions, c.VPNAddress)
	}
}

func TestSetupAsksForTheRefreshTimeOnlyWithAPackageManager(t *testing.T) {
	// Three Actions, then the refresh time; no Pairing code.
	h := newSetupHost(t, "\n\n\n03:30\n3:00\nn\n", "", false)
	h.packageManager = func() bool { return true }
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); c.RefreshTime != "03:00" {
		t.Errorf("refresh time = %q, want 03:00", c.RefreshTime)
	}
	if !strings.Contains(h.output.String(), "Enter a whole hour") || !strings.Contains(h.output.String(), "refreshed daily at 03:00") {
		t.Errorf("output:\n%s", h.output.String())
	}

	// Run again: Enter keeps it, none removes it.
	h.in = strings.NewReader("\n\n\n\nn\n")
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); c.RefreshTime != "03:00" {
		t.Errorf("refresh time = %q after Enter, want 03:00 kept", c.RefreshTime)
	}
	h.in = strings.NewReader("\n\n\nnone\nn\n")
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); c.RefreshTime != "" {
		t.Errorf("refresh time = %q after none, want none", c.RefreshTime)
	}

	// Without apt or dnf there is no refresh, so no question.
	h = newSetupHost(t, "\n\n\n\n", "", false)
	if err := h.setup(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.output.String(), "package list") {
		t.Errorf("setup asked about the refresh without a package manager:\n%s", h.output.String())
	}
}

func TestSetupFlagsFormSetsTheRefreshTime(t *testing.T) {
	h := newSetupHost(t, "", "", false)
	if err := h.setup(t, "--actions", "reboot", "--refresh-time", "4:00"); err != nil {
		t.Fatal(err)
	}
	if err := h.setup(t, "--pair"); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); c.RefreshTime != "04:00" || len(c.EnabledActions) != 1 {
		t.Errorf("refresh time = %q, enabled actions = %v; want 04:00 and reboot kept", c.RefreshTime, c.EnabledActions)
	}
	if err := h.setup(t, "--refresh-time", "04:30"); err == nil {
		t.Error("--refresh-time 04:30: no error")
	}
	if err := h.setup(t, "--refresh-time", "none"); err != nil {
		t.Fatal(err)
	}
	if c := h.config(t); c.RefreshTime != "" || len(c.EnabledActions) != 1 {
		t.Errorf("refresh time = %q, enabled actions = %v; want none and reboot kept", c.RefreshTime, c.EnabledActions)
	}
}

func TestStatusShowsActionsAndTheAgent(t *testing.T) {
	h := newSetupHost(t, "", "", false)
	var out bytes.Buffer
	h.out = &out
	if err := status(append(h.args(), "--config", h.configPath), h.owner); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Agent:", "Actions:     none", "sudo hostbeacon setup", "Port:        8743"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status has no %q:\n%s", want, out.String())
		}
	}
	if err := config.WriteSetup(h.configPath, config.Setup{EnabledActions: []protocol.Action{protocol.ActionUpdateRun}}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := status(append(h.args(), "--config", h.configPath), h.owner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Actions:     Update run") {
		t.Errorf("status does not show the Update run:\n%s", out.String())
	}
}

func mustAddr(text string) netip.Addr { return netip.MustParseAddr(text) }
