package helper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
	"github.com/iwaneo/hostbeacon/agent/internal/release"
	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
)

// The Agent update (v1 spec §10) runs in its own root systemd unit, so it
// goes on while it restarts both parts of the Agent. The unit runs the
// network part's program as root: the helper has no network code, and only
// starts the unit.
const (
	AgentUpdateUnit = "hostbeacon-agent-update.service"
	// DefaultAgentUpdateRecord is the record of the last Agent update. Root
	// writes it; the network part reads it and sends its result.
	DefaultAgentUpdateRecord = "/var/lib/hostbeacon-helper/agent-update.json"
	// DefaultAgentUpdateRequest passes the accepted Action ID to the unit.
	DefaultAgentUpdateRequest = "/run/hostbeacon-helper/agent-update-request.json"
	// DefaultAgentUpdateStaging holds the downloads and the package of the
	// installed version, for a rollback. Only root can read it.
	DefaultAgentUpdateStaging = "/var/lib/hostbeacon-helper/agent-update"
	// DefaultTarballDir holds one directory per version of a tarball install.
	DefaultTarballDir = "/usr/local/lib/hostbeacon"
)

// Agent update states in the record.
const (
	AgentUpdateRunning       = "running"
	AgentUpdateFinished      = "finished"
	AgentUpdateResultUnknown = "result_unknown"
)

const agentUpdateFormat = 1

// The units of both parts of the Agent, restarted after an install.
var agentUnits = []string{"hostbeacon-helper.service", "hostbeacon.service"}

// AgentUpdateRecord is the record of the last Agent update. Later releases
// may add fields; this one ignores them.
type AgentUpdateRecord struct {
	Format int `json:"format"`
	// ActionID is empty for the owner's `hostbeacon update`.
	ActionID    string `json:"action_id"`
	State       string `json:"state"`
	FromVersion string `json:"from_version"`
	// ToVersion is the version that runs after the update, also after a
	// rollback.
	ToVersion  *string `json:"to_version"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Result     *string `json:"result"` // ok or failed
	Error      *string `json:"error"`
	// Logged: the result is in the Action log.
	Logged bool `json:"logged"`
}

// Outcome is the Action result of an ended update; nil while it runs.
func (r *AgentUpdateRecord) Outcome() *protocol.ActionOutcome {
	switch {
	case r.State == AgentUpdateResultUnknown:
		return &protocol.ActionOutcome{Result: ResultFailed, Error: r.Error}
	case r.State != AgentUpdateFinished || r.Result == nil:
		return nil
	case *r.Result == ResultOK:
		return &protocol.ActionOutcome{Result: ResultOK}
	}
	return &protocol.ActionOutcome{Result: ResultFailed, Error: r.Error}
}

// resultAge is how long a connection gets the result of an ended update.
const resultAge = time.Hour

// ActionResult is the result to send Home Assistant: only for an update it
// asked for, and for an hour after it ended. nil when there is none.
func (r *AgentUpdateRecord) ActionResult(now time.Time) *protocol.ActionResult {
	outcome := r.Outcome()
	if r.ActionID == "" || outcome == nil || r.FinishedAt == nil {
		return nil
	}
	if finished, err := time.Parse(time.RFC3339, *r.FinishedAt); err != nil || now.Sub(finished) > resultAge {
		return nil
	}
	return &protocol.ActionResult{ActionID: r.ActionID, Action: protocol.ActionAgentUpdate, Result: outcome.Result, Error: outcome.Error}
}

// ReadAgentUpdate reads the record. It is nil when no update was made yet.
func ReadAgentUpdate(path string) (*AgentUpdateRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record AgentUpdateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if record.Format != agentUpdateFormat {
		return nil, fmt.Errorf("%s: format must be %d", path, agentUpdateFormat)
	}
	return &record, nil
}

func writeAgentUpdate(path string, record *AgentUpdateRecord) error {
	record.Format = agentUpdateFormat
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return statefile.Write(path, append(data, '\n'), 0o644)
}

// AgentUpdate is the Agent update unit's work (v1 spec §10).
type AgentUpdate struct {
	// Version is the installed version; Kind and Arch choose the file.
	Version string
	Kind    release.Kind
	Arch    string
	// Keys are the compiled-in release keys.
	Keys []release.PublicKey
	// Download gets a file of a release. The network part's program gives it.
	Download func(ctx context.Context, url string) ([]byte, error)
	// Run runs the package manager, systemctl, and a tarball's install.
	Run command.Command
	// TarballDir holds the versions of a tarball install.
	TarballDir      string
	Record          string
	Request         string
	Staging         string
	PackageTaskLock string
	Log             ActionLog
	// Ready tells systemd that the unit holds the package-task lock.
	Ready func() error
	// Healthy checks that both parts run version after a restart.
	Healthy func(ctx context.Context, version string) error
	Now     func() time.Time
	// LogWait is how long to wait for the helper to log the accepted
	// request; Poll is how often it is checked.
	LogWait, Poll time.Duration
}

// Start takes the package-task lock, tells systemd it is ready, and runs the
// Agent update the helper accepted, or the owner asked for. It returns
// ErrBusy when another package task holds the lock, and an error when
// nothing ran. An update that ran has its result in the record.
func (u *AgentUpdate) Start(ctx context.Context, cfg config.Config) error {
	data, err := os.ReadFile(u.Request)
	if err != nil {
		return fmt.Errorf("no Agent update request: %w", err)
	}
	// A request starts one update only.
	os.Remove(u.Request)
	var request runRequest
	if err := json.Unmarshal(data, &request); err != nil || request.ActionID != "" && !uuidPattern.MatchString(request.ActionID) {
		return errors.New("the Agent update request is not valid")
	}
	// A request from Home Assistant needs the Action on; the owner (root)
	// may always update. The unit re-checks the Host config itself.
	if request.ActionID != "" && !slices.Contains(cfg.EnabledActions, protocol.ActionAgentUpdate) {
		return errors.New("Agent update is turned off in the Host config")
	}
	if err := os.MkdirAll(filepath.Dir(u.PackageTaskLock), 0o755); err != nil {
		return err
	}
	unlock, err := lockPackageTask(u.PackageTaskLock)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrBusy
	}
	if err != nil {
		return fmt.Errorf("cannot take the package-task lock: %w", err)
	}
	defer unlock()
	if err := u.Ready(); err != nil {
		return err
	}
	// The helper logs the request as accepted after the unit is ready. An
	// Action that cannot be logged does not run.
	if request.ActionID != "" && !u.accepted(ctx, request.ActionID) {
		return errors.New("the Agent update request is not accepted in the Action log, so nothing runs")
	}
	record := &AgentUpdateRecord{
		ActionID:    request.ActionID,
		State:       AgentUpdateRunning,
		FromVersion: u.Version,
		StartedAt:   u.Now().UTC().Format(time.RFC3339),
	}
	if err := writeAgentUpdate(u.Record, record); err != nil {
		return err
	}
	running, err := u.update(ctx)
	record.ToVersion = &running
	u.finish(record, err)
	return nil
}

// accepted waits until the Action log holds actionID as an accepted request.
func (u *AgentUpdate) accepted(ctx context.Context, actionID string) bool {
	deadline := time.After(u.LogWait)
	for {
		if found, first, err := u.Log.find(actionID); err == nil && found && first == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-time.After(u.Poll):
		}
	}
}

// update installs the newest release. It returns the version that runs at
// the end, and why the update failed.
func (u *AgentUpdate) update(ctx context.Context) (running string, err error) {
	running = u.Version
	if !release.IsRelease(u.Version) {
		return running, fmt.Errorf("The installed Agent %s is not a release, so it cannot update itself. Install a release package by hand.", u.Version)
	}
	if err := os.MkdirAll(u.Staging, 0o700); err != nil {
		return running, err
	}
	// Only the file of the version that runs at the end stays.
	defer u.cleanStaging(&running)

	latest, err := u.sums(ctx, release.LatestURL(release.SumsFile), release.LatestURL(release.SignatureFile))
	if err != nil {
		return running, fmt.Errorf("Cannot check the newest release: %w", err)
	}
	switch order, err := release.Compare(latest.Version, u.Version); {
	case err != nil:
		return running, err
	case order < 0:
		return running, fmt.Errorf("The newest release %s is older than the installed %s. Hostbeacon never installs an older version.", latest.Version, u.Version)
	case order == 0:
		return running, nil
	}
	name := release.FileName(u.Kind, latest.Version, u.Arch)
	if !latest.Has(name) {
		return running, fmt.Errorf("Release %s has no %s", latest.Version, name)
	}
	// The rollback prerequisite: nothing is installed without a way back.
	previous, err := u.keepInstalled(ctx)
	if err != nil {
		return running, fmt.Errorf("Cannot keep the installed version %s for a rollback, so nothing was installed: %w", u.Version, err)
	}
	data, err := u.Download(ctx, release.VersionURL(latest.Version, name))
	if err != nil {
		return running, fmt.Errorf("Cannot download %s: %w", name, err)
	}
	if err := latest.Check(name, data); err != nil {
		return running, err
	}
	file := filepath.Join(u.Staging, name)
	if err := statefile.Write(file, data, 0o600); err != nil {
		return running, err
	}

	if err := u.install(ctx, file); err != nil {
		failed := fmt.Errorf("The install failed: %w", err)
		// Often the install changed nothing, and the old version still runs.
		if u.Healthy(ctx, u.Version) == nil {
			return running, fmt.Errorf("%w. Version %s still runs.", failed, u.Version)
		}
		return u.rollback(ctx, previous, failed)
	}
	if err := u.Healthy(ctx, latest.Version); err != nil {
		return u.rollback(ctx, previous, fmt.Errorf("Version %s did not pass the health check (%w)", latest.Version, err))
	}
	running = latest.Version
	if u.Kind == release.Tarball {
		os.RemoveAll(filepath.Join(u.TarballDir, u.Version))
	}
	return running, nil
}

// sums downloads and verifies a release's SHA256SUMS.
func (u *AgentUpdate) sums(ctx context.Context, sumsURL, signatureURL string) (release.Sums, error) {
	data, err := u.Download(ctx, sumsURL)
	if err != nil {
		return release.Sums{}, err
	}
	signature, err := u.Download(ctx, signatureURL)
	if err != nil {
		return release.Sums{}, err
	}
	return release.ReadSums(data, signature, u.Keys)
}

// keepInstalled makes sure the installed version can be installed again, and
// returns what the rollback installs. A package the staging directory lacks
// (the first update after a manual install) comes from its own release.
func (u *AgentUpdate) keepInstalled(ctx context.Context) (string, error) {
	if u.Kind == release.Tarball {
		program := filepath.Join(u.TarballDir, u.Version, "hostbeacon")
		if _, err := os.Stat(program); err != nil {
			return "", err
		}
		return program, nil
	}
	name := release.FileName(u.Kind, u.Version, u.Arch)
	file := filepath.Join(u.Staging, name)
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	sums, err := u.sums(ctx, release.VersionURL(u.Version, release.SumsFile), release.VersionURL(u.Version, release.SignatureFile))
	if err != nil {
		return "", err
	}
	if sums.Version != u.Version {
		return "", fmt.Errorf("the release of %s is signed as %s", u.Version, sums.Version)
	}
	data, err := u.Download(ctx, release.VersionURL(u.Version, name))
	if err != nil {
		return "", err
	}
	if err := sums.Check(name, data); err != nil {
		return "", err
	}
	return file, statefile.Write(file, data, 0o600)
}

// install installs the new version's file and restarts both parts.
func (u *AgentUpdate) install(ctx context.Context, file string) error {
	var err error
	switch u.Kind {
	case release.Deb:
		// Waits up to 5 minutes for another apt or dpkg.
		_, err = u.Run(ctx, "apt-get", "install", "-y", "-q", "-o", "DPkg::Lock::Timeout=300", file)
	case release.RPM:
		// The package needs nothing from a repository.
		_, err = u.Run(ctx, "dnf", "install", "-y", "-q", "--disablerepo=*", file)
	case release.Tarball:
		var program string
		if program, err = unpackTarball(file, u.Staging); err == nil {
			// The new version's install copies it into its own directory and
			// switches the one symlink to it.
			_, err = u.Run(ctx, program, "install")
			os.RemoveAll(filepath.Dir(program))
		}
	}
	if err != nil {
		return withStderr(err)
	}
	return u.restart(ctx)
}

// rollback installs the previous version again after a failed install or
// health check, and returns the version that runs and the error to report.
func (u *AgentUpdate) rollback(ctx context.Context, previous string, cause error) (string, error) {
	var err error
	switch u.Kind {
	case release.Deb:
		_, err = u.Run(ctx, "apt-get", "install", "-y", "-q", "-o", "DPkg::Lock::Timeout=300", "--allow-downgrades", previous)
	case release.RPM:
		_, err = u.Run(ctx, "dnf", "downgrade", "-y", "-q", "--disablerepo=*", previous)
	case release.Tarball:
		_, err = u.Run(ctx, previous, "install")
	}
	if err == nil {
		err = u.restart(ctx)
	}
	if err == nil {
		err = u.Healthy(ctx, u.Version)
	}
	if err != nil {
		return u.Version, fmt.Errorf("%w, and the rollback to %s failed too: %v", cause, u.Version, withStderr(err))
	}
	return u.Version, fmt.Errorf("%w, so the Agent went back to %s", cause, u.Version)
}

func (u *AgentUpdate) restart(ctx context.Context) error {
	_, err := u.Run(ctx, "systemctl", append([]string{"restart"}, agentUnits...)...)
	return withStderr(err)
}

// cleanStaging removes every file but the package of the running version.
func (u *AgentUpdate) cleanStaging(running *string) {
	keep := release.FileName(u.Kind, *running, u.Arch)
	entries, _ := os.ReadDir(u.Staging)
	for _, entry := range entries {
		if u.Kind == release.Tarball || entry.Name() != keep {
			os.RemoveAll(filepath.Join(u.Staging, entry.Name()))
		}
	}
}

// finish stores the result in the record and the Action log.
func (u *AgentUpdate) finish(record *AgentUpdateRecord, failure error) {
	record.State = AgentUpdateFinished
	record.FinishedAt = ptr(u.Now().UTC().Format(time.RFC3339))
	record.Result = ptr(ResultOK)
	if failure != nil {
		record.Result = ptr(ResultFailed)
		record.Error = ptr(shorten(failure.Error()))
	}
	u.save(record)
	if logAgentUpdate(u.Log, record) == nil {
		record.Logged = true
		u.save(record)
	}
}

func (u *AgentUpdate) save(record *AgentUpdateRecord) {
	if err := writeAgentUpdate(u.Record, record); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write the Agent update record:", err)
	}
}

// EndAgentUpdate completes the record after the unit ended. An update that
// stopped without a result (the Host restarted, or the unit was killed) is
// result_unknown, and its result goes to the Action log. Call it only when
// the unit is not running.
func EndAgentUpdate(path string, log ActionLog, now func() time.Time) error {
	record, err := ReadAgentUpdate(path)
	if err != nil || record == nil {
		return err
	}
	if record.State == AgentUpdateRunning {
		record.State = AgentUpdateResultUnknown
		record.FinishedAt = ptr(now().UTC().Format(time.RFC3339))
		record.Error = ptr("The Agent update stopped before it reported a result.")
		if err := writeAgentUpdate(path, record); err != nil {
			return err
		}
	}
	if record.Logged {
		return nil
	}
	if err := logAgentUpdate(log, record); err != nil {
		return err
	}
	record.Logged = true
	return writeAgentUpdate(path, record)
}

// logAgentUpdate writes the result of Home Assistant's request to the Action
// log. The owner's update is not an Action.
func logAgentUpdate(log ActionLog, record *AgentUpdateRecord) error {
	outcome := record.Outcome()
	if record.ActionID == "" || outcome == nil {
		return nil
	}
	return log.write(logEntry{Entry: "result", ActionID: record.ActionID, Action: protocol.ActionAgentUpdate, Result: outcome.Result, Error: outcome.Error})
}

// unpackTarball unpacks a release tarball into a directory in dir, and
// returns the path of its hostbeacon program. Only the programs and the
// license are taken.
func unpackTarball(file, dir string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	zipped, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	top := strings.TrimSuffix(filepath.Base(file), ".tar.gz")
	target := filepath.Join(dir, top)
	os.RemoveAll(target)
	if err := os.Mkdir(target, 0o700); err != nil {
		return "", err
	}
	archive := tar.NewReader(zipped)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name, found := strings.CutPrefix(header.Name, top+"/")
		if !found || header.Typeflag != tar.TypeReg || !slices.Contains([]string{"hostbeacon", "hostbeacon-helper", "LICENSE"}, name) {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(archive, 256<<20))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(target, name), content, 0o700); err != nil {
			return "", err
		}
	}
	program := filepath.Join(target, "hostbeacon")
	if _, err := os.Stat(program); err != nil {
		return "", errors.New("the tarball has no hostbeacon program")
	}
	return program, nil
}

// SystemdHealth checks that both parts of the Agent run a version after a
// restart, and stay up.
type SystemdHealth struct {
	Run command.Command
	// Program is the installed hostbeacon, which says its version.
	Program string
	// Listening checks that the network part accepts connections; nil
	// skips it. The helper package has no network code, so the caller gives it.
	Listening func(ctx context.Context) error
	// Start is how long both parts may take to start; Stable, how long they
	// must then run without a restart. Poll is how often they are checked.
	Start, Stable, Poll time.Duration
}

// Check returns nil when both parts run version and did not restart.
func (h SystemdHealth) Check(ctx context.Context, version string) error {
	deadline := time.Now().Add(h.Start)
	var pids []string
	for {
		var err error
		if pids, err = h.running(ctx, version); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		if err := sleep(ctx, h.Poll); err != nil {
			return err
		}
	}
	if err := sleep(ctx, h.Stable); err != nil {
		return err
	}
	now, err := h.running(ctx, version)
	if err != nil {
		return err
	}
	if !slices.Equal(now, pids) {
		return errors.New("a part of the Agent restarted during the health check")
	}
	return nil
}

// running returns the main process IDs of both parts when both are active
// and the installed program is version.
func (h SystemdHealth) running(ctx context.Context, version string) ([]string, error) {
	out, err := h.Run(ctx, h.Program, "version")
	if got := strings.TrimSpace(string(out)); err != nil || got != "hostbeacon "+version {
		return nil, fmt.Errorf("the installed program is %q, not %s", got, version)
	}
	out, err = h.Run(ctx, "systemctl", append([]string{"show", "--property=ActiveState,MainPID"}, agentUnits...)...)
	if err != nil {
		return nil, withStderr(err)
	}
	blocks := strings.Split(strings.TrimSpace(string(out)), "\n\n")
	if len(blocks) != len(agentUnits) {
		return nil, errors.New("cannot read the state of the Agent's units")
	}
	var pids []string
	for i, block := range blocks {
		properties := map[string]string{}
		for line := range strings.Lines(block) {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			properties[key] = value
		}
		if properties["ActiveState"] != "active" || properties["MainPID"] == "" || properties["MainPID"] == "0" {
			return nil, fmt.Errorf("%s is %s", agentUnits[i], properties["ActiveState"])
		}
		pids = append(pids, properties["MainPID"])
	}
	if h.Listening != nil {
		if err := h.Listening(ctx); err != nil {
			return nil, fmt.Errorf("the network part does not accept connections: %w", err)
		}
	}
	return pids, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
