package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
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
	} {
		if _, err := load(t, content); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: no error")
	}
}
