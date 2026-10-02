// Command hostbeacon is the Hostbeacon Agent.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/discovery"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/hugepages"
	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/install"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
	"github.com/iwaneo/hostbeacon/agent/internal/release"
	"github.com/iwaneo/hostbeacon/agent/internal/server"
	"github.com/iwaneo/hostbeacon/agent/internal/system"
	"github.com/iwaneo/hostbeacon/agent/internal/version"
)

const (
	defaultStateDir = "/var/lib/hostbeacon"
	defaultCacheDir = "/var/cache/hostbeacon"
)

const usage = `Usage:
  hostbeacon setup    turn Actions on or off, allow Home Assistant's VPN address,
                      set the refresh time, open the firewall port, and pair
                      (run as root). It asks each question. With flags it asks
                      nothing and changes only what the flags name:
                        --actions reboot,update_run,agent_update | none
                                                         (the others are turned off)
                        --vpn-address <address> | none   (unchanged if not given)
                        --refresh-time <hour> | none     (daily package list refresh,
                                                         such as 03:00; none: once
                                                         every 24 hours)
                        --open-firewall                  (firewalld or ufw)
                        --pair                           (show a Pairing code)
  hostbeacon pair     show a Pairing code for Home Assistant (run as root)
  hostbeacon pairings list
                      show each Home Assistant paired with this Host (run as root)
  hostbeacon pairings remove <ID or name>
                      remove a Pairing and close its connection (run as root)
  hostbeacon status   show whether the Agent runs, its Actions, identity, and
                      Pairings (run as root)
  hostbeacon reset-identity [--yes]
                      give this Host a new identity, as for a copy; removes every
                      Pairing (run as root). Clone detection is best effort: run it
                      on every copy of a Host before it goes online.
  hostbeacon keep-identity [--drop-missing]
                      keep this Host's identity and end an identity hold (run as root)
  hostbeacon regenerate-key [--yes]
                      make a new key and certificate; removes every Pairing (run as root)
  hostbeacon update   update the Agent to the newest release, as Home Assistant's
                      Install does: checks the release signature, and goes back
                      to this version if the new one does not start (run as root)
  hostbeacon install  install the Agent from the unpacked tarball, into
                      /usr/local (run as root, from the tarball directory)
  hostbeacon serve    run the network part (systemd starts it)
  hostbeacon version  show the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(version.String())
		fmt.Print(usage)
		return
	}
	var err error
	switch command, args := os.Args[1], os.Args[2:]; command {
	case "serve":
		err = serve(args)
	case "pair":
		err = pair(args)
	case "pairings":
		err = pairings(args, os.Stdout, time.Now(), time.Local)
	case "agent-update":
		// The Agent update unit's main process; it checks for root itself.
		err = agentUpdateUnit(args)
	case "agent-update-ended":
		err = agentUpdateEnded(args)
	case "setup", "status", "update", "install", "reset-identity", "keep-identity", "regenerate-key":
		if os.Geteuid() != 0 {
			err = errors.New("run it as root: sudo hostbeacon " + command)
			break
		}
		run := map[string]func([]string, owner) error{
			"setup":          setup,
			"status":         status,
			"update":         updateAgent,
			"install":        installTarball,
			"reset-identity": resetIdentity,
			"keep-identity":  keepIdentity,
			"regenerate-key": regenerateKey,
		}[command]
		err = run(args, systemOwner())
	case "version", "--version":
		fmt.Println(version.String())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", command, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostbeacon:", err)
		os.Exit(1)
	}
}

// installTarball installs the Agent from the directory this program is in.
func installTarball(args []string, o owner) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	tarball := install.Tarball{Root: "/", Source: filepath.Dir(self), Version: version.Version, Run: o.run, Out: o.out}
	return tarball.Install(context.Background())
}

func pair(args []string) error {
	flags := flag.NewFlagSet("pair", flag.ExitOnError)
	stateDir := flags.String("state-dir", defaultStateDir, "the Agent's state directory")
	flags.Parse(args)

	return printPairingCode(os.Stdout, *stateDir)
}

// printPairingCode makes a new Pairing code and shows it.
func printPairingCode(out io.Writer, stateDir string) error {
	code, expires, err := pairing.NewCode(stateDir, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, `Pairing code: %s

In Home Assistant, add the Hostbeacon integration and enter this code.
It works once, until %s (10 minutes), and stops after 5 wrong tries.
A new code replaces this one.
`, code, expires.Local().Format("15:04"))
	return nil
}

const pairingsUsage = "usage: hostbeacon pairings list | hostbeacon pairings remove <ID or name>"

// pairings lists or removes Pairings. A removed Pairing's key stops working at
// once, and the running Agent closes its connection within a second.
func pairings(args []string, out io.Writer, now time.Time, zone *time.Location) error {
	if len(args) == 0 {
		return errors.New(pairingsUsage)
	}
	flags := flag.NewFlagSet("pairings "+args[0], flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultStateDir, "the Agent's state directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	store := pairing.Open(*stateDir, time.Now)
	switch {
	case args[0] == "list" && flags.NArg() == 0:
		items, err := store.List()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Fprintln(out, "No Pairings. Run `sudo hostbeacon pair` to pair Home Assistant.")
			return nil
		}
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tNAME\tCREATED\tLAST SEEN")
		const layout = "2006-01-02 15:04"
		for _, p := range items {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Created.In(zone).Format(layout), p.LastSeen.In(zone).Format(layout))
		}
		table.Flush()
		for _, p := range pairing.Stale(items, now) {
			fmt.Fprintf(out, "\nWarning: Pairing %s (%s) was not seen for 90 days or more. If that Home Assistant is gone, remove it:\n  sudo hostbeacon pairings remove %s\n", p.ID, p.Name, p.ID)
		}
		return nil
	case args[0] == "remove" && flags.NArg() == 1:
		found, err := store.Find(flags.Arg(0))
		if err != nil {
			return err
		}
		removed, err := store.Remove(found.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed the Pairing %s (%s). Its Home Assistant cannot connect any more.\n", removed.Name, removed.ID)
		return nil
	}
	return errors.New(pairingsUsage)
}

// warnStalePairings logs each Pairing not seen for 90 days or more, now and
// then once a day. Old Pairings are never removed by themselves.
func warnStalePairings(ctx context.Context, log *slog.Logger, pairings *pairing.Pairings) {
	for {
		list, err := pairings.List()
		if err != nil {
			log.Error("cannot read the Pairings", "error", err)
		}
		for _, p := range pairing.Stale(list, time.Now()) {
			log.Warn("a Pairing was not seen for 90 days or more; if its Home Assistant is gone, run: hostbeacon pairings remove "+p.ID,
				"pairing", p.Name, "id", p.ID, "last_seen", p.LastSeen)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", config.DefaultPath, "the Host config file")
	stateDir := flags.String("state-dir", defaultStateDir, "the Agent's state directory")
	cacheDir := flags.String("cache-dir", defaultCacheDir, "a directory the package manager may use for its cache")
	helperSocket := flags.String("helper", helper.DefaultSocket, "the root helper's socket")
	flags.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := hugepages.Disable(); err != nil {
		log.Warn("cannot turn off transparent huge pages", "error", err)
	}

	// These groups would give the network part root's reach (v1 spec §4.1).
	if groups := forbiddenGroups(groupNames()); len(groups) > 0 {
		return fmt.Errorf("the Agent user must not be in the group %s", strings.Join(groups, ", "))
	}

	// Fail closed: without a readable Host config, accept no connections.
	hostConfig, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("cannot read the Host config, so no connections are accepted: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The clone check, before any connection is accepted (v1 spec §4.4).
	helperClient := helper.Client{Socket: *helperSocket}
	signals := readSignals(ctx, "/", func(ctx context.Context) (*string, error) { return readSMBIOSWithRetry(ctx, helperClient) })
	id, check, err := identity.Start(*stateDir, signals)
	if err != nil {
		return fmt.Errorf("cannot load the Agent identity: %w", err)
	}
	switch check.Outcome {
	case identity.Copied:
		log.Warn("this Host is a copy of another Host: the Agent made a new identity and removed every Pairing", "instance_id", id.InstanceID, "copied_from", id.CopiedFrom)
		if err := helperClient.LogIdentityCopy(ctx, helper.IdentityCopy{InstanceID: id.InstanceID, CopiedFrom: id.CopiedFrom}); err != nil {
			log.Error("cannot write the identity copy to the Action log", "error", err)
		}
	case identity.Hold:
		log.Error("identity hold: the Agent accepts no connections, because it cannot read an identity signal it read at install; run `sudo hostbeacon status` to see what to do", "signals", check.Missing)
		<-ctx.Done()
		return nil
	}
	if len(signals.Unreadable) > 0 {
		log.Warn("cannot read an identity signal; it is not checked", "signals", signals.Unreadable)
	}

	// The package manager may write its cache and log under HOME (dnf5 does).
	// Without this directory, only reading Available updates on dnf fails.
	if err := os.MkdirAll(*cacheDir, 0o700); err != nil {
		log.Warn("cannot make the cache directory", "error", err)
	}
	run := system.Exec([]string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + *cacheDir})
	var services system.ServiceSource
	var tasks system.PackageTasks
	if systemd, err := system.ConnectSystemd(ctx); err != nil {
		log.Warn("cannot read systemd over D-Bus, so failed services and package tasks are not shown", "error", err)
	} else {
		defer systemd.Close()
		services, tasks = systemd, systemd
	}
	if _, err := os.Stat(*helperSocket); err != nil {
		log.Warn("cannot reach the root helper, so SMART and containers are not shown", "error", err)
	}
	host := system.Detect(ctx, "/", run, system.Statfs, services, helper.Client{Socket: *helperSocket}, time.Now)
	host.Tasks = tasks
	host.RefreshSchedule = hostConfig.RefreshSchedule()
	hostname, _ := os.Hostname()
	agent := protocol.AgentInfo{
		Hostname:     hostname,
		AgentVersion: version.Version,
		Capabilities: host.Capabilities,
		// Home Assistant shows a control only for an enabled Action. The root
		// helper re-reads the Host config for every request.
		EnabledActions: hostConfig.EnabledActions,
	}
	collector := system.NewCollector(host, system.DefaultIntervals, agent)
	time.Sleep(time.Second) // the first CPU value needs an earlier reading
	state := server.NewState(collector.Sample(ctx))
	collector.Run(ctx, state.Set)
	keys, err := release.ParseKeys(release.PublicKeys)
	if err != nil {
		return err
	}
	go checkNewestVersion(ctx, log, keys, func(newest string) { collector.SetNewestAgentVersion(newest, state.Set) })
	go watchAgentUpdate(ctx, log, state, helper.DefaultAgentUpdateRecord)

	pairings := pairing.Open(*stateDir, time.Now)
	go warnStalePairings(ctx, log, pairings)
	s := &server.Server{
		Config:   hostConfig,
		Identity: id,
		Pairings: pairings,
		State:    state,
		Hello: protocol.HelloRequest{
			InstanceID:     id.InstanceID,
			RunID:          identity.NewUUID(),
			CopiedFrom:     append([]string{}, id.CopiedFrom...),
			Hostname:       agent.Hostname,
			AgentVersion:   agent.AgentVersion,
			Capabilities:   agent.Capabilities,
			EnabledActions: agent.EnabledActions,
			Environment:    host.Environment,
			Distro: protocol.Distro{
				ID:      optional(host.Release["ID"]),
				Name:    optional(host.Release["NAME"]),
				Version: optional(host.Release["VERSION_ID"]),
			},
			Architecture: runtime.GOARCH,
			Kernel:       host.Kernel,
		},
		Actions: helperClient,
		KnownID: func(hostID string) error { return identity.AddKnownID(*stateDir, hostID) },
		Log:     log,
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(hostConfig.Port)))
	if err != nil {
		return err
	}
	log.Info("listening", "port", hostConfig.Port, "instance_id", id.InstanceID, "fingerprint", fmt.Sprintf("%x", id.Fingerprint))
	go func() {
		name := cmp.Or(hostname, "Hostbeacon Agent")
		if err := discovery.Announce(ctx, name, id.InstanceID, hostConfig.Port); err != nil && ctx.Err() == nil {
			log.Warn("cannot announce the Agent on the local network; add the Host in Home Assistant by address", "error", err)
		}
	}()
	return s.Serve(ctx, listener)
}

// readSMBIOSWithRetry asks the root helper for the SMBIOS UUID. At boot the
// helper may start a little later, so it tries for up to 30 seconds before the
// signal counts as unreadable.
func readSMBIOSWithRetry(ctx context.Context, client helper.Client) (*string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		uuid, err := client.ReadSMBIOSUUID(ctx)
		if err == nil {
			return uuid, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
	}
}

// forbiddenGroups returns the groups in names that give access to Docker,
// raw disks, or all logs.
func forbiddenGroups(names []string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool {
		return !slices.Contains([]string{"docker", "disk", "adm"}, name)
	})
}

// groupNames are the names of this process's groups.
func groupNames() []string {
	gids, _ := os.Getgroups()
	gids = append(gids, os.Getegid())
	var names []string
	for _, gid := range gids {
		if group, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
			names = append(names, group.Name)
		}
	}
	return names
}

// optional is null for empty text.
func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}
