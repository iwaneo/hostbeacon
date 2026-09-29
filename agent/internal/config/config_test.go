package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

func load(t *testing.T, content string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func allows(c Config, address string) bool {
	return c.Allows(netip.MustParseAddr(address))
}

func TestDefaultsAllowOnlyPrivateRanges(t *testing.T) {
	c, err := load(t, `{"format": 1}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != DefaultPort {
		t.Errorf("port = %d, want %d", c.Port, DefaultPort)
	}
	if len(c.EnabledActions) != 0 {
		t.Errorf("enabled actions = %v, want none: every Action is off until the owner turns it on", c.EnabledActions)
	}
	if !c.PackageListRefresh {
		t.Error("package list refresh is off, want on by default")
	}
	for _, address := range []string{"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.5", "127.0.0.1", "::1", "fd00::1", "fe80::1", "::ffff:192.168.1.5"} {
		if !allows(c, address) {
			t.Errorf("%s is refused, want allowed", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "172.32.0.1", "100.64.0.1", "2001:db8::1", "::ffff:8.8.8.8"} {
		if allows(c, address) {
			t.Errorf("%s is allowed, want refused", address)
		}
	}
}

func TestOwnerCanSetAddressesAndPort(t *testing.T) {
	c, err := load(t, `{"format": 1, "port": 9000, "allowed_sources": ["192.168.1.0/24", "100.64.1.2"], "later_field": true}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 {
		t.Errorf("port = %d, want 9000", c.Port)
	}
	if !allows(c, "100.64.1.2") || !allows(c, "192.168.1.9") {
		t.Error("an address in the list is refused")
	}
	if allows(c, "100.64.1.3") || allows(c, "10.0.0.1") {
		t.Error("an address outside the list is allowed")
	}
}

func TestOwnerCanEnableActionsAndTurnOffRefresh(t *testing.T) {
	c, err := load(t, `{"format": 1, "enabled_actions": ["reboot", "agent_update"], "package_list_refresh": false}`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionReboot, protocol.ActionAgentUpdate}) {
		t.Errorf("enabled actions = %v", c.EnabledActions)
	}
	if c.PackageListRefresh {
		t.Error("package list refresh is on, want off")
	}
}

func TestActionFromALaterReleaseIsNotEnabled(t *testing.T) {
	// After a rollback, the config may name an Action this release does not
	// know. It stays off, and the rest of the config still works.
	c, err := load(t, `{"format": 1, "enabled_actions": ["reboot", "shutdown"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionReboot}) {
		t.Errorf("enabled actions = %v, want only reboot", c.EnabledActions)
	}
}

func TestRootOwnedRefusesAFileOthersCanChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"format": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		// The test user owns the file, not root.
		if _, err := LoadRootOwned(path); err == nil {
			t.Error("a file not owned by root is accepted")
		}
		return
	}
	if _, err := LoadRootOwned(path); err != nil {
		t.Fatalf("a root-owned file is refused: %v", err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRootOwned(path); err == nil {
		t.Error("a group-writable file is accepted")
	}
}

func TestEmptyListAllowsNothing(t *testing.T) {
	c, err := load(t, `{"format": 1, "allowed_sources": []}`)
	if err != nil {
		t.Fatal(err)
	}
	if allows(c, "127.0.0.1") {
		t.Error("an empty list allows loopback")
	}
}

func TestConfigThatCannotBeReadFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":       `{format: 1`,
		"no format":      `{}`,
		"unknown format": `{"format": 2}`,
		"bad address":    `{"format": 1, "allowed_sources": ["everyone"]}`,
		"bad port":       `{"format": 1, "port": 70000}`,
		"refresh text":   `{"format": 1, "package_list_refresh": "no"}`,
	} {
		if _, err := load(t, content); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: no error")
	}
}

func TestVPNAddressIsAllowedBesideTheList(t *testing.T) {
	c, err := load(t, `{"format": 1, "vpn_address": "100.101.102.103"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !allows(c, "100.101.102.103") || !allows(c, "10.0.0.1") {
		t.Error("the VPN address or a private address is refused")
	}
	if allows(c, "100.101.102.104") {
		t.Error("another VPN address is allowed")
	}
	for _, bad := range []string{`"100.64.0.0/10"`, `"everyone"`, `5`} {
		if _, err := load(t, `{"format": 1, "vpn_address": `+bad+`}`); err == nil {
			t.Errorf("vpn_address %s: no error, want only a single address", bad)
		}
	}
}

func TestWriteSetupMakesALoadableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostbeacon", "config.json")
	vpn := "100.101.102.103"
	if err := WriteSetup(path, Setup{EnabledActions: []protocol.Action{protocol.ActionReboot}, VPNAddress: &vpn}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionReboot}) {
		t.Errorf("enabled actions = %v, want reboot", c.EnabledActions)
	}
	if !allows(c, vpn) {
		t.Error("the VPN address is refused")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644: only root may change the config", info.Mode().Perm())
	}
}

func TestWriteSetupKeepsWhatItDoesNotSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"format": 1, "port": 9000, "enabled_actions": ["reboot"], "vpn_address": "100.64.1.2", "later_field": {"a": 1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// No VPN address given: the old one stays.
	if err := WriteSetup(path, Setup{EnabledActions: []protocol.Action{protocol.ActionUpdateRun}}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || !allows(c, "100.64.1.2") {
		t.Errorf("port = %d, VPN address allowed = %v: want both kept", c.Port, allows(c, "100.64.1.2"))
	}
	if !slices.Equal(c.EnabledActions, []protocol.Action{protocol.ActionUpdateRun}) {
		t.Errorf("enabled actions = %v, want only update_run: an Action not named is off", c.EnabledActions)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"later_field"`) {
		t.Errorf("a field from a later release was dropped:\n%s", data)
	}

	// An empty VPN address removes it.
	empty := ""
	if err := WriteSetup(path, Setup{VPNAddress: &empty}); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if allows(c, "100.64.1.2") || len(c.EnabledActions) != 0 {
		t.Errorf("VPN address allowed = %v, enabled actions = %v: want neither", allows(c, "100.64.1.2"), c.EnabledActions)
	}
}

func TestWriteSetupRefusesWhatLoadWouldRefuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"format": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"100.64.0.0/10", "not an address"} {
		if err := WriteSetup(path, Setup{VPNAddress: &bad}); err == nil {
			t.Errorf("VPN address %q: no error", bad)
		}
	}
	if err := WriteSetup(path, Setup{EnabledActions: []protocol.Action{"shutdown"}}); err == nil {
		t.Error("unknown Action: no error")
	}
	data, _ := os.ReadFile(path)
	if string(data) != `{"format": 1}` {
		t.Errorf("a refused setup changed the file:\n%s", data)
	}
}

func TestWriteDefaultKeepsAnOwnerConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostbeacon", "config.json")
	created, err := WriteDefault(path)
	if err != nil || !created {
		t.Fatalf("created = %v, err = %v; want a new file", created, err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EnabledActions) != 0 {
		t.Errorf("enabled actions = %v, want none after install", c.EnabledActions)
	}
	if err := os.WriteFile(path, []byte(`{"format": 1, "port": 9000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if created, err := WriteDefault(path); err != nil || created {
		t.Fatalf("created = %v, err = %v; want the owner's file kept", created, err)
	}
	if c, _ := Load(path); c.Port != 9000 {
		t.Error("the owner's config was replaced")
	}
}
