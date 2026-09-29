// Command hostbeacon-helper is the Agent's root helper. It has no network
// code: the network part reaches it only through a local socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/version"
)

const usage = `Usage:
  hostbeacon-helper serve                 run the root helper (systemd starts it as root)
  hostbeacon-helper refresh-package-list  refresh the package list if it is older than 24 hours
                                          (the root timer runs it)
  hostbeacon-helper update-run            run the Update run the helper accepted
                                          (the Update run unit runs it)
  hostbeacon-helper update-run-ended      complete the Update run record after the unit
                                          ended (the Update run unit runs it)
  hostbeacon-helper version               show the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(version.String())
		fmt.Print(usage)
		return
	}
	var err error
	switch name, args := os.Args[1], os.Args[2:]; name {
	case "serve":
		err = serve(args)
	case "refresh-package-list":
		err = refreshPackageList(args)
	case "update-run":
		err = updateRun(args)
	case "update-run-ended":
		err = updateRunEnded(args)
	case "version", "--version":
		fmt.Println(version.String())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", name, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostbeacon-helper:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", config.DefaultPath, "the Host config file")
	socket := flags.String("socket", helper.DefaultSocket, "the socket the network part connects to")
	agentUser := flags.String("user", "hostbeacon", "the user the network part runs as; no other user may connect")
	actionLog := flags.String("action-log", helper.DefaultActionLogDir, "the Action log directory")
	flags.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	account, err := user.Lookup(*agentUser)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	loadConfig := func() (config.Config, error) { return config.LoadRootOwned(*configPath) }
	if _, err := loadConfig(); err != nil {
		// Not fatal: every request is refused until the owner fixes it.
		log.Error("cannot read the Host config, so every request is refused", "error", err)
	}

	listener, err := listen(*socket, gid)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	env := []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	run := command.Exec(env)
	updateRuns := helper.SystemdUpdateRun{Run: run, Request: helper.DefaultUpdateRunRequest}
	// A run that ended while the helper was down, for example at a power
	// loss, gets its result here (v1 spec §11).
	if active, err := updateRuns.Active(ctx); err != nil {
		log.Error("cannot read the Update run unit", "error", err)
	} else if !active {
		if err := helper.EndUpdateRun(helper.DefaultUpdateRunRecord, helper.ActionLog{Dir: *actionLog, Now: time.Now}); err != nil {
			log.Error("cannot complete the Update run record", "error", err)
		}
	}
	server := &helper.Server{
		AllowedUID: uid,
		LoadConfig: loadConfig,
		Jobs:       helper.HostJobs{Root: "/", Run: run, Stream: command.ExecStream(env)},
		Actions: &helper.ActionRunner{
			Log:                    helper.ActionLog{Dir: *actionLog, Now: time.Now},
			Journal:                log,
			Uptime:                 func() (time.Duration, error) { return helper.ReadUptime("/") },
			PackageTaskLock:        helper.DefaultPackageTaskLock,
			PackageManagerLocks:    helper.DefaultPackageManagerLocks,
			PackageManagerPIDLocks: helper.DefaultPackageManagerPIDLocks,
			PackageManager:         helper.DetectPackageManager("/"),
			UpdateRuns:             updateRuns,
			UpdateRunRecord:        helper.DefaultUpdateRunRecord,
			// An orderly reboot, with no delay (v1 spec §9).
			Reboot: func(ctx context.Context) error {
				_, err := run(ctx, "systemctl", "reboot")
				return err
			},
		},
		Log: log,
	}
	log.Info("listening", "socket", *socket, "user", *agentUser)
	return server.Serve(ctx, listener)
}

// refreshPackageList refreshes the package list for the root timer (v1 spec
// §4.6). A skipped turn is not an error; a failed refresh is, so systemd
// shows the unit as failed.
func refreshPackageList(args []string) error {
	flags := flag.NewFlagSet("refresh-package-list", flag.ExitOnError)
	configPath := flags.String("config", config.DefaultPath, "the Host config file")
	flags.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	// Fail closed: without a readable Host config, nothing runs.
	cfg, err := config.LoadRootOwned(*configPath)
	if err != nil {
		return fmt.Errorf("cannot read the Host config, so the package list is not refreshed: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	refresh := helper.PackageListRefresh{
		Manager: helper.DetectPackageManager("/"),
		// systemd stops the unit sooner if it hangs.
		Run:                    command.ExecFor([]string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}, time.Hour),
		Stamp:                  helper.DefaultPackageListStamp,
		PackageTaskLock:        helper.DefaultPackageTaskLock,
		PackageManagerLocks:    helper.DefaultPackageManagerLocks,
		PackageManagerPIDLocks: helper.DefaultPackageManagerPIDLocks,
		Now:                    time.Now,
	}
	result, err := refresh.Refresh(ctx, cfg)
	if err != nil {
		return err
	}
	log.Info("package list refresh", "result", result)
	return nil
}

// updateRun is the Update run unit's main process (v1 spec §8). It exits
// with exitBusy before it is ready when another package task holds the
// package-task lock, so the helper refuses the request as busy.
func updateRun(args []string) error {
	flags := flag.NewFlagSet("update-run", flag.ExitOnError)
	configPath := flags.String("config", config.DefaultPath, "the Host config file")
	actionLog := flags.String("action-log", helper.DefaultActionLogDir, "the Action log directory")
	flags.Parse(args)

	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	// Fail closed: without a readable Host config, nothing runs.
	cfg, err := config.LoadRootOwned(*configPath)
	if err != nil {
		return fmt.Errorf("cannot read the Host config, so the Update run does not start: %w", err)
	}
	env := []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		// Default answers, and no pager or prompt.
		"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}
	run := helper.UpdateRun{
		Manager: helper.DetectPackageManager("/"),
		// Never stopped, however long (v1 spec §8).
		Run:                    command.ExecFor(env, 0),
		Stream:                 command.ExecStream(env),
		Root:                   "/",
		Record:                 helper.DefaultUpdateRunRecord,
		Request:                helper.DefaultUpdateRunRequest,
		Stamp:                  helper.DefaultPackageListStamp,
		Downloads:              helper.DefaultUpdateRunDownloads,
		PackageTaskLock:        helper.DefaultPackageTaskLock,
		PackageManagerLocks:    helper.DefaultPackageManagerLocks,
		PackageManagerPIDLocks: helper.DefaultPackageManagerPIDLocks,
		Log:                    helper.ActionLog{Dir: *actionLog, Now: time.Now},
		Ready:                  notifyReady,
		Now:                    time.Now,
		LockWait:               5 * time.Minute,
		LogWait:                30 * time.Second,
		Poll:                   time.Second,
	}
	// Hostbeacon never stops a run. When systemd stops the unit (at
	// shutdown), the unit's end marks the result unknown.
	err = run.Start(context.Background(), cfg)
	if errors.Is(err, helper.ErrBusy) {
		fmt.Fprintln(os.Stderr, "hostbeacon-helper:", err)
		os.Exit(exitBusy)
	}
	return err
}

// exitBusy is EX_TEMPFAIL.
const exitBusy = 75

// updateRunEnded runs after the Update run unit's main process ended, also
// when it was killed.
func updateRunEnded(args []string) error {
	flags := flag.NewFlagSet("update-run-ended", flag.ExitOnError)
	actionLog := flags.String("action-log", helper.DefaultActionLogDir, "the Action log directory")
	flags.Parse(args)
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	return helper.EndUpdateRun(helper.DefaultUpdateRunRecord, helper.ActionLog{Dir: *actionLog, Now: time.Now})
}

// notifyReady tells systemd that the unit is ready (sd_notify READY=1).
func notifyReady() error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return errors.New("NOTIFY_SOCKET is not set: only the Update run unit runs an Update run")
	}
	// A name starting with @ is an abstract socket; Go handles it.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1"))
	return err
}

// listen makes the socket. Only root and the Agent's group can connect;
// the helper then checks the caller's user as well.
func listen(socket string, gid int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chown(socket, 0, gid); err != nil {
		listener.Close()
		return nil, err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
