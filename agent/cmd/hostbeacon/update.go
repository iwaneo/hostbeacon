package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
	"github.com/iwaneo/hostbeacon/agent/internal/release"
	"github.com/iwaneo/hostbeacon/agent/internal/server"
	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
	"github.com/iwaneo/hostbeacon/agent/internal/version"
)

const (
	// maxDownload limits a release file; a package is about 10 MB.
	maxDownload = 256 << 20
	// versionCheckInterval: the newest Agent version is checked once a day
	// (v1 spec §4.6).
	versionCheckInterval = 24 * time.Hour
	// agentUpdatePoll is how often the network part reads the Agent update
	// record.
	agentUpdatePoll = 5 * time.Second
	// exitBusy is EX_TEMPFAIL: another package task holds the lock.
	exitBusy = 75
)

// download gets one file of a release from the URL fixed in the program.
func download(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", version.String())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownload+1))
	if err == nil && len(data) > maxDownload {
		err = fmt.Errorf("%s is too large", url)
	}
	return data, err
}

// newestVersion reads the version of the newest release from its signed
// SHA256SUMS. An unsigned version is never shown.
func newestVersion(ctx context.Context, get func(context.Context, string) ([]byte, error), keys []release.PublicKey) (string, error) {
	sums, err := get(ctx, release.LatestURL(release.SumsFile))
	if err != nil {
		return "", err
	}
	signature, err := get(ctx, release.LatestURL(release.SignatureFile))
	if err != nil {
		return "", err
	}
	checked, err := release.ReadSums(sums, signature, keys)
	return checked.Version, err
}

// checkNewestVersion checks the newest version at start and then once a day.
// A failed check keeps the last known version.
func checkNewestVersion(ctx context.Context, log *slog.Logger, keys []release.PublicKey, found func(string)) {
	for {
		newest, err := newestVersion(ctx, download, keys)
		if err != nil {
			log.Warn("cannot check the newest Agent version", "error", err)
		} else {
			found(newest)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(versionCheckInterval):
		}
	}
}

// watchAgentUpdate passes the result of the last Agent update Home Assistant
// asked for to every connection. The update restarts the Agent, so a new
// connection gets the result of the update that ran before it.
func watchAgentUpdate(ctx context.Context, log *slog.Logger, state *server.State, path string) {
	for {
		record, err := helper.ReadAgentUpdate(path)
		if err != nil {
			log.Warn("cannot read the Agent update record", "error", err)
		}
		var result *protocol.ActionResult
		if record != nil {
			result = record.ActionResult(time.Now())
		}
		state.SetAgentUpdateResult(result)
		select {
		case <-ctx.Done():
			return
		case <-time.After(agentUpdatePoll):
		}
	}
}

// updateAgent is `hostbeacon update`: it starts the Agent update unit, as
// Home Assistant's Install does, waits until it ends, and shows the result.
// The owner may update also when Agent update is off in the Host config.
func updateAgent(args []string, o owner) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(helper.DefaultAgentUpdateRequest), 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"action_id": ""})
	if err := statefile.Write(helper.DefaultAgentUpdateRequest, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintln(o.out, "Checking for a new Agent version...")
	started := time.Now()
	if _, err := o.run(ctx, "systemctl", "start", helper.AgentUpdateUnit); err != nil {
		return fmt.Errorf("the Agent update did not start. Another package task (an Update run, the package list refresh, or an Agent update) may run; try again later. Details: sudo journalctl -u %s", helper.AgentUpdateUnit)
	}
	for {
		out, _ := o.run(ctx, "systemctl", "is-active", helper.AgentUpdateUnit)
		if state := strings.TrimSpace(string(out)); state != "active" && state != "activating" && state != "deactivating" {
			break
		}
		time.Sleep(time.Second)
	}
	record, err := helper.ReadAgentUpdate(helper.DefaultAgentUpdateRecord)
	if err != nil {
		return err
	}
	// The record must be of this update, not an earlier one.
	if record == nil || record.ActionID != "" || !startedAfter(record.StartedAt, started) {
		return fmt.Errorf("the Agent update ended without a result. Details: sudo journalctl -u %s", helper.AgentUpdateUnit)
	}
	to := record.FromVersion
	if record.ToVersion != nil {
		to = *record.ToVersion
	}
	switch outcome := record.Outcome(); {
	case outcome == nil || outcome.Result != helper.ResultOK:
		reason := "it stopped before it reported a result"
		if record.Error != nil {
			reason = *record.Error
		}
		return fmt.Errorf("the Agent update failed: %s", reason)
	case to == record.FromVersion:
		fmt.Fprintf(o.out, "The Agent is up to date (%s).\n", to)
	default:
		fmt.Fprintf(o.out, "Updated the Agent from %s to %s.\n", record.FromVersion, to)
	}
	return nil
}

// startedAfter says whether the RFC 3339 time text is at or after t, to the second.
func startedAfter(text string, t time.Time) bool {
	when, err := time.Parse(time.RFC3339, text)
	return err == nil && !when.Before(t.Truncate(time.Second))
}

// agentUpdateUnit is the Agent update unit's main process (v1 spec §10). It
// exits with exitBusy before it is ready when another package task holds the
// package-task lock, so the helper refuses the request as busy.
func agentUpdateUnit(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	// Fail closed: without a readable Host config, nothing runs.
	cfg, err := config.LoadRootOwned(config.DefaultPath)
	if err != nil {
		return fmt.Errorf("cannot read the Host config, so the Agent update does not start: %w", err)
	}
	keys, err := release.ParseKeys(release.PublicKeys)
	if err != nil {
		return err
	}
	kind, program, err := installKind()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Default answers, and no pager or prompt.
	run := command.ExecFor([]string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}, 30*time.Minute)
	update := helper.AgentUpdate{
		Version:         version.Version,
		Kind:            kind,
		Arch:            runtime.GOARCH,
		Keys:            keys,
		Download:        download,
		Run:             run,
		TarballDir:      helper.DefaultTarballDir,
		Record:          helper.DefaultAgentUpdateRecord,
		Request:         helper.DefaultAgentUpdateRequest,
		Staging:         helper.DefaultAgentUpdateStaging,
		PackageTaskLock: helper.DefaultPackageTaskLock,
		Log:             helper.ActionLog{Dir: helper.DefaultActionLogDir, Now: time.Now},
		Ready:           helper.NotifyReady,
		Healthy: helper.SystemdHealth{
			Run:     run,
			Program: program,
			Start:   time.Minute,
			// Longer than the units' RestartSec, so a part that crashes
			// right after it started is seen.
			Stable: 20 * time.Second,
			Poll:   time.Second,
		}.Check,
		Now:     time.Now,
		LogWait: 30 * time.Second,
		Poll:    time.Second,
	}
	err = update.Start(ctx, cfg)
	if errors.Is(err, helper.ErrBusy) {
		fmt.Fprintln(os.Stderr, "hostbeacon:", err)
		os.Exit(exitBusy)
	}
	return err
}

// agentUpdateEnded runs after the Agent update unit's main process ended,
// also when it was killed.
func agentUpdateEnded(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	return helper.EndAgentUpdate(helper.DefaultAgentUpdateRecord, helper.ActionLog{Dir: helper.DefaultActionLogDir, Now: time.Now}, time.Now)
}

// installKind says how the Agent was installed, and which program the
// health check asks for the installed version.
func installKind() (release.Kind, string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", "", err
	}
	switch {
	case strings.HasPrefix(self, helper.DefaultTarballDir+"/"):
		return release.Tarball, "/usr/local/bin/hostbeacon", nil
	case self != "/usr/bin/hostbeacon":
		return "", "", fmt.Errorf("the Agent at %s was not installed from a Hostbeacon package or tarball, so it cannot update itself", self)
	}
	if _, err := os.Stat("/var/lib/dpkg/info/hostbeacon.list"); err == nil {
		return release.Deb, self, nil
	}
	if exec.Command("rpm", "-q", "--quiet", "hostbeacon").Run() == nil {
		return release.RPM, self, nil
	}
	return "", "", errors.New("cannot tell whether the Agent was installed from the .deb or the .rpm")
}
