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
	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
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
	// VPNAddress is Home Assistant's single VPN address, if the owner set
	// one. It is in AllowedSources too.
	VPNAddress netip.Addr
}

type file struct {
	Format             int               `json:"format"`
	Port               *int              `json:"port"`
	AllowedSources     *[]string         `json:"allowed_sources"`
	EnabledActions     []protocol.Action `json:"enabled_actions"`
	PackageListRefresh *bool             `json:"package_list_refresh"`
	VPNAddress         *string           `json:"vpn_address"`
}

var actions = []protocol.Action{protocol.ActionReboot, protocol.ActionUpdateRun, protocol.ActionAgentUpdate}

// Load reads the Host config at path. Fields a later release adds are
// ignored, so a rollback keeps working.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c, err := parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func parse(data []byte) (Config, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return Config{}, err
	}
	if f.Format != fileFormat {
		return Config{}, fmt.Errorf("format must be %d", fileFormat)
	}
	c := Config{Port: DefaultPort, EnabledActions: []protocol.Action{}, PackageListRefresh: true}
	if f.Port != nil {
		if *f.Port < 1 || *f.Port > 65535 {
			return Config{}, fmt.Errorf("port %d is not a TCP port", *f.Port)
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
			return Config{}, fmt.Errorf("allowed_sources: %w", err)
		}
		c.AllowedSources = append(c.AllowedSources, prefix)
	}
	// Home Assistant's single VPN address, allowed beside the list.
	if f.VPNAddress != nil {
		addr, err := netip.ParseAddr(*f.VPNAddress)
		if err != nil {
			return Config{}, fmt.Errorf("vpn_address must be a single address: %w", err)
		}
		c.VPNAddress = addr
		c.AllowedSources = append(c.AllowedSources, netip.PrefixFrom(addr, addr.BitLen()))
	}
	// An Action this release does not know stays off.
	for _, action := range f.EnabledActions {
		if slices.Contains(actions, action) && !slices.Contains(c.EnabledActions, action) {
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

// Default is the Host config an install writes: every Action off.
const Default = "{\n  \"format\": 1\n}\n"

// WriteDefault writes the default Host config at path, unless a config is
// there already. It says whether it wrote one.
func WriteDefault(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, write(path, []byte(Default))
}

// Setup is what `hostbeacon setup` sets in the Host config.
type Setup struct {
	// EnabledActions replaces the enabled Actions: an Action not named is off.
	EnabledActions []protocol.Action
	// VPNAddress is Home Assistant's single VPN address. Nil keeps the
	// current one; empty removes it.
	VPNAddress *string
}

// WriteSetup writes s into the Host config at path, or into a new default
// config. It keeps every other field, also those of a later release, and
// changes nothing when the result would not load.
func WriteSetup(path string, s Setup) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data, err = []byte(Default), nil
	}
	if err != nil {
		return err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, action := range s.EnabledActions {
		if !slices.Contains(actions, action) {
			return fmt.Errorf("unknown Action %q", action)
		}
	}
	if fields["enabled_actions"], err = json.Marshal(append([]protocol.Action{}, s.EnabledActions...)); err != nil {
		return err
	}
	switch {
	case s.VPNAddress == nil:
	case *s.VPNAddress == "":
		delete(fields, "vpn_address")
	default:
		if fields["vpn_address"], err = json.Marshal(*s.VPNAddress); err != nil {
			return err
		}
	}
	data, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := parse(data); err != nil {
		return err
	}
	return write(path, data)
}

// write replaces the file at path in one step. Only root may change it.
func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return statefile.Write(path, data, 0o644)
}
