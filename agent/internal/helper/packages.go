package helper

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// planned is one package an Update run installs: its name, architecture,
// and full new version (with epoch and release). New: it is not installed
// yet (apt only).
type planned struct {
	Name, Arch, Version string
	New                 bool
}

// transaction is what the Update run's checks approved (v1 spec §8).
type transaction struct {
	Installs []planned
	// Removes are the packages apt would remove. dnf replaces obsoleted
	// packages as normal, so its runs have none.
	Removes []string
	// Files are the packages dnf downloaded for the run.
	Files []string
}

// parseAptSimulation reads the Inst and Remv lines of apt-get -s, like
// "Inst tzdata [2026b] (2026c Debian:13.7/stable, Debian-Security:13/stable-security [all])".
func parseAptSimulation(out string) transaction {
	var plan transaction
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, _, _ := strings.Cut(fields[1], ":")
		switch fields[0] {
		case "Remv":
			plan.Removes = append(plan.Removes, name)
		case "Inst":
			_, details, found := strings.Cut(line, "(")
			open, end := strings.LastIndex(details, "["), strings.LastIndex(details, "]")
			version := strings.Fields(details)
			if !found || open < 0 || end < open || len(version) == 0 {
				continue
			}
			// An upgrade shows the installed version next: "[2026b]".
			upgrade := len(fields) > 2 && strings.HasPrefix(fields[2], "[")
			plan.Installs = append(plan.Installs, planned{Name: name, Arch: details[open+1 : end], Version: version[0], New: !upgrade})
		}
	}
	return plan
}

// aptInstallArgs names each approved package with its architecture and exact
// version, so apt installs those versions or nothing.
func aptInstallArgs(installs []planned) []string {
	args := make([]string, 0, len(installs))
	for _, item := range installs {
		args = append(args, item.Name+":"+item.Arch+"="+item.Version)
	}
	return args
}

// aptReleaseChanges says whether the enabled package indexes could move the
// Host to another distro release: every index from Debian or Ubuntu must be
// for the Host's codename, or its -updates, -security, or -backports suite.
// apt-get update removes the lists of sources that are no longer enabled, so
// the Release files in lists are the enabled indexes. With no index read, or
// no codename, it fails closed.
func aptReleaseChanges(lists, codename string) (bool, error) {
	files, err := filepath.Glob(filepath.Join(lists, "*Release"))
	if err != nil {
		return false, err
	}
	if len(files) == 0 {
		return false, errors.New("no package index was found")
	}
	allowed := []string{codename, codename + "-updates", codename + "-security", codename + "-backports"}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			return false, err
		}
		fields := releaseFields(string(data))
		origin := fields["Origin"]
		if !strings.HasPrefix(origin, "Debian") && !strings.HasPrefix(origin, "Ubuntu") {
			continue
		}
		if codename == "" || !slices.Contains(allowed, fields["Codename"]) {
			return true, nil
		}
	}
	return false, nil
}

// releaseFields reads the first value of each field of a Release file. The
// lines of the hash lists start with a space, so they are skipped.
func releaseFields(data string) map[string]string {
	fields := map[string]string{}
	for line := range strings.Lines(data) {
		key, value, found := strings.Cut(strings.TrimRight(line, "\r\n"), ": ")
		if found && !strings.HasPrefix(key, " ") && fields[key] == "" {
			fields[key] = strings.TrimSpace(value)
		}
	}
	return fields
}

// rpmHeaderFormat is the rpm query format parseRPMHeaders reads.
const rpmHeaderFormat = "%{NAME} %{ARCH} %{EPOCHNUM}:%{VERSION}-%{RELEASE}\n"

func parseRPMHeaders(out string) []planned {
	var packages []planned
	for line := range strings.Lines(out) {
		if fields := strings.Fields(line); len(fields) == 3 {
			packages = append(packages, planned{Name: fields[0], Arch: fields[1], Version: fields[2]})
		}
	}
	return packages
}

// dnfReleaseChanges says whether the run would change the major version of
// a package that owns /etc/os-release. owners holds each owner's name and
// installed version; with no owner, it fails closed.
func dnfReleaseChanges(owners, installs []planned) bool {
	if len(owners) == 0 {
		return true
	}
	for _, owner := range owners {
		for _, item := range installs {
			if item.Name != owner.Name {
				continue
			}
			// Epoch, then version, then release: "0:44-27" has version 44.
			_, version, _ := strings.Cut(item.Version, ":")
			version, _, _ = strings.Cut(version, "-")
			if major(version) != major(owner.Version) {
				return true
			}
		}
	}
	return false
}

func major(version string) string {
	before, _, _ := strings.Cut(version, ".")
	return before
}

// aptAutoMarks lists the approved packages to mark as automatically
// installed after the install: apt marks every package named on its command
// line as manually installed, so autoremove would never remove them. Those
// that were automatically installed before, and the new ones (dependencies
// of the upgrades, like a new kernel), are marked again. auto is the output
// of apt-mark showauto.
func aptAutoMarks(installs []planned, auto string) []string {
	wasAuto := map[string]bool{}
	for line := range strings.Lines(auto) {
		wasAuto[strings.TrimSpace(line)] = true
	}
	var marks []string
	for _, item := range installs {
		if item.New || wasAuto[item.Name] || wasAuto[item.Name+":"+item.Arch] {
			marks = append(marks, item.Name+":"+item.Arch)
		}
	}
	return marks
}

// countAptUpgradable counts the lines of apt list --upgradable.
func countAptUpgradable(out string) int64 {
	var count int64
	for line := range strings.Lines(out) {
		if strings.Contains(line, "[upgradable from:") {
			count++
		}
	}
	return count
}

// parseOSRelease reads the KEY=value lines of /etc/os-release.
func parseOSRelease(data string) map[string]string {
	values := map[string]string{}
	for line := range strings.Lines(data) {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || strings.HasPrefix(key, "#") {
			continue
		}
		values[key] = strings.Trim(value, `"'`)
	}
	return values
}
