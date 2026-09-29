package system

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// PackageSystemBroken says whether the package system needs a repair, and
// the command that repairs it or shows what is wrong (v1 spec §6.3, §7.7).
// Reading needs no root. ok is false when it cannot tell.
func PackageSystemBroken(ctx context.Context, run Command, manager string) (broken bool, fix *string, ok bool) {
	var checks []packageCheck
	switch manager {
	case "apt":
		checks = []packageCheck{
			// Packages left half installed, for example by a stopped install.
			{fix: "sudo dpkg --configure -a", args: []string{"dpkg", "--audit"}, output: true},
			// Unmet dependencies.
			{fix: "sudo apt-get --fix-broken install", args: []string{"apt-get", "check", "-qq", "-o", "Debug::NoLocking=1"}},
		}
	case "dnf":
		checks = []packageCheck{{fix: "sudo dnf check", args: []string{"dnf", "-q", "check"}}}
	default:
		return false, nil, false
	}
	for _, check := range checks {
		out, err := run(ctx, check.args[0], check.args[1:]...)
		var exit *exec.ExitError
		if errors.As(err, &exit) || err == nil && check.output && strings.TrimSpace(string(out)) != "" {
			return true, &check.fix, true
		}
		if err != nil {
			return false, nil, false
		}
	}
	return false, nil, true
}

// packageCheck fails when its command exits with an error, or, with output,
// when it prints anything.
type packageCheck struct {
	fix    string
	args   []string
	output bool
}
