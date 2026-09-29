// Package config reads the Host config. Root owns the file; the network part
// and Home Assistant cannot change it. If it cannot be read or understood,
// the Agent accepts no connections (fail closed).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// DefaultPath is where the Host config lives.
const DefaultPath = "/etc/hostbeacon/config.json"

// DefaultPort is the TCP port the Agent listens on.
const DefaultPort = 8743

const fileFormat = 1

// defaultSources are the private ranges and loopback. 100.64.0.0/10
// (Tailscale, also used by internet providers) is not in the list.
var defaultSources = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
	"fc00::/7", "fe80::/10", "::1/128",
}

// Config is the Host config.
type Config struct {
	Port           int
	AllowedSources []netip.Prefix
	// EnabledActions is empty until the owner turns an Action on.
	EnabledActions []protocol.Action
	// PackageListRefresh is the daily package list refresh (v1 spec §4.6).
	PackageListRefresh bool
}

type file struct {
	Format             int               `json:"format"`
	Port               *int              `json:"port"`
	AllowedSources     *[]string         `json:"allowed_sources"`
	EnabledActions     []protocol.Action `json:"enabled_actions"`
	PackageListRefresh *bool             `json:"package_list_refresh"`
}

var actions = []protocol.Action{protocol.ActionReboot, protocol.ActionUpdateRun, protocol.ActionAgentUpdate}

// Load reads the Host config at path. Fields a later release adds are
// ignored, so a rollback keeps working.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Format != fileFormat {
		return Config{}, fmt.Errorf("%s: format must be %d", path, fileFormat)
	}
	c := Config{Port: DefaultPort, EnabledActions: []protocol.Action{}, PackageListRefresh: true}
	if f.Port != nil {
		if *f.Port < 1 || *f.Port > 65535 {
			return Config{}, fmt.Errorf("%s: port %d is not a TCP port", path, *f.Port)
		}
		c.Port = *f.Port
	}
	sources := defaultSources
	if f.AllowedSources != nil {
		sources = *f.AllowedSources
	}
	for _, source := range sources {
		prefix, err := parseSource(source)
		if err != nil {
			return Config{}, fmt.Errorf("%s: allowed_sources: %w", path, err)
		}
		c.AllowedSources = append(c.AllowedSources, prefix)
	}
	for _, action := range f.EnabledActions {
		if !slices.Contains(actions, action) {
			return Config{}, fmt.Errorf("%s: enabled_actions: unknown Action %q", path, action)
		}
		if !slices.Contains(c.EnabledActions, action) {
			c.EnabledActions = append(c.EnabledActions, action)
		}
	}
	if f.PackageListRefresh != nil {
		c.PackageListRefresh = *f.PackageListRefresh
	}
	return c, nil
}

// LoadRootOwned is Load for the root helper. It also refuses the file when
// root does not own it and its directory, or when others can change them.
func LoadRootOwned(path string) (Config, error) {
	for _, name := range []string{filepath.Dir(path), path} {
		info, err := os.Stat(name)
		if err != nil {
			return Config{}, err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return Config{}, errors.New("cannot read the owner of " + name)
		}
		if owner.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return Config{}, fmt.Errorf("%s must be owned by root and writable only by root", name)
		}
	}
	return Load(path)
}

// Enabled says whether the owner turned action on.
func (c Config) Enabled(action protocol.Action) bool {
	return slices.Contains(c.EnabledActions, action)
}

// parseSource reads a range such as 10.0.0.0/8, or a single address.
func parseSource(source string) (netip.Prefix, error) {
	if strings.Contains(source, "/") {
		prefix, err := netip.ParsePrefix(source)
		return prefix.Masked(), err
	}
	addr, err := netip.ParseAddr(source)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Allows says whether a connection from addr may be accepted.
func (c Config) Allows(addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	for _, prefix := range c.AllowedSources {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
