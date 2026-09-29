package helper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// DefaultPackageTaskLock is the Hostbeacon package-task lock (v1 spec §8),
// taken with flock by the Update run, the Agent update, and the package list
// refresh.
const DefaultPackageTaskLock = "/run/hostbeacon-helper/package-task.lock"

// DefaultPackageManagerLocks are the locks apt, dpkg, rpm, and dnf5 hold
// while they change the Host or refresh the package list. They lock with
// fcntl. A missing file is not held.
var DefaultPackageManagerLocks = []string{
	"/var/lib/dpkg/lock-frontend",
	"/var/lib/dpkg/lock",
	"/var/lib/apt/lists/lock",
	"/var/cache/apt/archives/lock",
	"/usr/lib/sysimage/rpm/.rpm.lock",
	"/var/lib/rpm/.rpm.lock",
	"/run/dnf/rpmtransaction.lock",
}

// minUptime: no Reboot within 10 minutes of boot (v1 spec §9).
const minUptime = 10 * time.Minute

const maxUser = 256

// ActionRequest is an Action request as the network part passes it on. The
// Pairing and the user are the network part's and Home Assistant's claims.
type ActionRequest struct {
	ActionID    string          `json:"action_id"`
	Action      protocol.Action `json:"action"`
	PairingID   string          `json:"pairing_id"`
	PairingName string          `json:"pairing_name"`
	User        *string         `json:"user"` // nil: no HA user
}

// Ack is the helper's answer to an Action request.
type Ack struct {
	Status      string                  `json:"status"` // accepted or refused
	Reason      *protocol.RefusalReason `json:"reason"`
	FirstResult *protocol.ActionOutcome `json:"first_result"`
}

// ActionRunner checks, logs, and runs Actions. It re-checks every guard
// itself, so a bug in the network part can only ask for what the owner
// enabled (v1 spec §4.1).
type ActionRunner struct {
	Log ActionLog
	// Journal gets every request as well; systemd keeps it in the journal.
	Journal             *slog.Logger
	Uptime              func() (time.Duration, error)
	PackageTaskLock     string
	PackageManagerLocks []string
	Reboot              func(ctx context.Context) error

	// mu makes the Action ID check and the log write one step.
	mu sync.Mutex
}

// Request checks one Action request and writes it to the Action log. When it
// is accepted, run does the Action and logs its result. An error means the
// request itself is invalid; it is not logged in the Action log.
func (r *ActionRunner) Request(cfg config.Config, request ActionRequest) (ack Ack, run func(context.Context) protocol.ActionOutcome, err error) {
	if err := request.check(); err != nil {
		r.Journal.Warn("refused an invalid Action request", "error", err)
		return Ack{}, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	reason, first, release := r.guard(cfg, request)
	entry := logEntry{
		Entry:     "request",
		ActionID:  request.ActionID,
		Action:    request.Action,
		PairingID: request.PairingID,
		Pairing:   request.PairingName,
		User:      ptr(noHAUser),
		Status:    "accepted",
		Reason:    reason,
	}
	if request.User != nil {
		// The user name is Home Assistant's claim. Any text is logged, as
		// JSON, and cut to a length.
		user := []rune(*request.User)
		entry.User = ptr(string(user[:min(len(user), maxUser)]))
	}
	if reason != "" {
		entry.Status = "refused"
	}
	if err := r.Log.write(entry); err != nil {
		r.Journal.Error("cannot write the Action log", "error", err)
		if release != nil {
			release()
		}
		entry.Status, entry.Reason, first, release = "refused", protocol.ReasonCannotLog, nil, nil
	}
	r.Journal.Info("Action request", "action_id", entry.ActionID, "action", entry.Action, "pairing", entry.Pairing,
		"pairing_id", entry.PairingID, "user", *entry.User, "status", entry.Status, "reason", entry.Reason)
	if entry.Status == "refused" {
		return Ack{Status: "refused", Reason: &entry.Reason, FirstResult: first}, nil, nil
	}
	return Ack{Status: "accepted"}, func(ctx context.Context) protocol.ActionOutcome {
		defer release()
		return r.reboot(ctx, request)
	}, nil
}

// guard returns why the request is refused, or "" when it may run. For an
// accepted Reboot, release frees the package-task lock it holds.
func (r *ActionRunner) guard(cfg config.Config, request ActionRequest) (reason protocol.RefusalReason, first *protocol.ActionOutcome, release func()) {
	found, first, err := r.Log.find(request.ActionID)
	if err != nil {
		r.Journal.Error("cannot read the Action log", "error", err)
		return protocol.ReasonCannotLog, nil, nil
	}
	if found {
		return protocol.ReasonDuplicate, first, nil
	}
	// Update run and Agent update come in later releases of this helper.
	if !slices.Contains(cfg.EnabledActions, request.Action) || request.Action != protocol.ActionReboot {
		return protocol.ReasonDisabled, nil, nil
	}
	uptime, err := r.Uptime()
	if err != nil {
		r.Journal.Error("cannot read the uptime, so Reboot is refused", "error", err)
		return protocol.ReasonTooSoonAfterBoot, nil, nil
	}
	if uptime < minUptime {
		return protocol.ReasonTooSoonAfterBoot, nil, nil
	}
	release, err = lockPackageTask(r.PackageTaskLock)
	if err != nil {
		r.Journal.Info("Reboot refused: a package task holds the package-task lock", "error", err)
		return protocol.ReasonBusy, nil, nil
	}
	if held, err := anyLockHeld(r.PackageManagerLocks); held || err != nil {
		r.Journal.Info("Reboot refused: the package manager is busy", "lock", held, "error", err)
		release()
		return protocol.ReasonBusy, nil, nil
	}
	return "", nil, release
}

func (r *ActionRunner) reboot(ctx context.Context, request ActionRequest) protocol.ActionOutcome {
	outcome := protocol.ActionOutcome{Result: "ok"}
	if err := r.Reboot(ctx); err != nil {
		text := err.Error()
		outcome = protocol.ActionOutcome{Result: "failed", Error: &text}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := logEntry{Entry: "result", ActionID: request.ActionID, Action: request.Action, Result: outcome.Result, Error: outcome.Error}
	if err := r.Log.write(entry); err != nil {
		r.Journal.Error("cannot write an Action result to the Action log", "action_id", request.ActionID, "error", err)
	}
	r.Journal.Info("Action result", "action_id", request.ActionID, "action", request.Action, "result", outcome.Result, "error", outcome.Error)
	return outcome
}

func (request ActionRequest) check() error {
	switch {
	case !uuidPattern.MatchString(strings.ToLower(request.ActionID)):
		return errors.New("the Action ID is not a UUID")
	case !slices.Contains([]protocol.Action{protocol.ActionReboot, protocol.ActionUpdateRun, protocol.ActionAgentUpdate}, request.Action):
		return fmt.Errorf("unknown Action %q", request.Action)
	case !printable(request.PairingID, 64) || !printable(request.PairingName, 64):
		return errors.New("the Pairing is missing or not printable text")
	}
	return nil
}

func ptr[T any](value T) *T { return &value }

func printable(text string, maxRunes int) bool {
	runes := []rune(text)
	return len(runes) > 0 && len(runes) <= maxRunes && !slices.ContainsFunc(runes, func(r rune) bool { return !unicode.IsPrint(r) })
}

// lockPackageTask takes the package-task lock without waiting.
func lockPackageTask(path string) (release func(), err error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	// Closing the file frees the lock.
	return func() { file.Close() }, nil
}

// anyLockHeld says whether another process holds an fcntl lock on one of
// paths. It only asks; it never takes the lock.
func anyLockHeld(paths []string) (bool, error) {
	for _, path := range paths {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK}
		err = syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &lock)
		file.Close()
		if err != nil {
			return false, err
		}
		if lock.Type != syscall.F_UNLCK {
			return true, nil
		}
	}
	return false, nil
}

// ReadUptime reads how long the Host (or the container) has been up.
func ReadUptime(root string) (time.Duration, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("/proc/uptime is empty")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
