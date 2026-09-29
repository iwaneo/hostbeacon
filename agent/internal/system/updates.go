package system

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

const maxNames = 100

type update struct {
	name, arch, installed, available string
}

// ReadAvailableUpdates reads the Available updates from the local package
// cache, without root. It never refreshes the package list. The last
// refresh time stays null here; the Collector adds it.
func ReadAvailableUpdates(ctx context.Context, run Command, manager string) (protocol.AvailableUpdates, error) {
	var updates []update
	var err error
	switch manager {
	case "apt":
		updates, err = readApt(ctx, run)
	case "dnf":
		updates, err = readDnf(ctx, run)
	default:
		err = errors.New("no supported package manager")
	}
	if err != nil {
		return protocol.AvailableUpdates{}, err
	}
	slices.SortFunc(updates, func(a, b update) int {
		return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.arch, b.arch))
	})
	hash := sha256.New()
	result := protocol.AvailableUpdates{Packages: []protocol.Package{}}
	for _, item := range updates {
		hash.Write([]byte(item.name + " " + item.arch + " " + item.installed + " " + item.available + "\n"))
		if len(result.Packages) < maxNames {
			result.Packages = append(result.Packages, protocol.Package{Name: item.name, InstalledVersion: item.installed, NewVersion: item.available})
		}
	}
	count := int64(len(updates))
	fingerprint := hex.EncodeToString(hash.Sum(nil))
	result.Count, result.Fingerprint = &count, &fingerprint
	return result, nil
}

// readApt parses lines like
// "base-files/stable 13.8+deb13u7 arm64 [upgradable from: 13.0]".
func readApt(ctx context.Context, run Command) ([]update, error) {
	out, err := run(ctx, "apt", "list", "--upgradable", "-o", "APT::Cmd::Disable-Script-Warning=true")
	if err != nil {
		return nil, err
	}
	var updates []update
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 6 || fields[3] != "[upgradable" || fields[4] != "from:" {
			continue
		}
		name, _, _ := strings.Cut(fields[0], "/")
		updates = append(updates, update{name: name, arch: fields[2], installed: strings.TrimSuffix(fields[5], "]"), available: fields[1]})
	}
	return updates, nil
}

// readDnf reads the newest version of each upgradable package, then the
// installed versions from rpm. dnf reads only its cache here.
func readDnf(ctx context.Context, run Command) ([]update, error) {
	out, err := run(ctx, "dnf", "--cacheonly", "--quiet", "repoquery", "--upgrades", "--latest-limit=1", "--queryformat", `%{name} %{arch} %{evr}\n`)
	if err != nil {
		return nil, err
	}
	installedOut, err := run(ctx, "rpm", "-qa", "--queryformat", `%{NAME} %{ARCH} %{EVR}\n`)
	if err != nil {
		return nil, err
	}
	installed := map[[2]string]string{}
	for line := range strings.Lines(string(installedOut)) {
		if fields := strings.Fields(line); len(fields) == 3 {
			key := [2]string{fields[0], fields[1]}
			if current, ok := installed[key]; !ok || compareVersions(fields[2], current) > 0 {
				installed[key] = fields[2]
			}
		}
	}
	var updates []update
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if version, ok := installed[[2]string{fields[0], fields[1]}]; ok {
			updates = append(updates, update{name: fields[0], arch: fields[1], installed: version, available: fields[2]})
		}
	}
	return updates, nil
}
