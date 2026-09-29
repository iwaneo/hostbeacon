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
	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
	"github.com/iwaneo/hostbeacon/agent/internal/server"
	"github.com/iwaneo/hostbeacon/agent/internal/system"
	"github.com/iwaneo/hostbeacon/agent/internal/version"
)

const (
	defaultStateDir = "/var/lib/hostbeacon"
	defaultCacheDir = "/var/cache/hostbeacon"
)

const usage = `Usage:
  hostbeacon serve    run the network part (systemd starts it)
  hostbeacon pair     show a Pairing code for Home Assistant (run as root)
  hostbeacon pairings list
                      show each Home Assistant paired with this Host (run as root)
  hostbeacon pairings remove <ID or name>
                      remove a Pairing and close its connection (run as root)
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

func pair(args []string) error {
	flags := flag.NewFlagSet("pair", flag.ExitOnError)
	stateDir := flags.String("state-dir", defaultStateDir, "the Agent's state directory")
	flags.Parse(args)

	code, expires, err := pairing.NewCode(*stateDir, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf(`Pairing code: %s

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

	// These groups would give the network part root's reach (v1 spec §4.1).
	if groups := forbiddenGroups(groupNames()); len(groups) > 0 {
		return fmt.Errorf("the Agent user must not be in the group %s", strings.Join(groups, ", "))
	}

	// Fail closed: without a readable Host config, accept no connections.
	hostConfig, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("cannot read the Host config, so no connections are accepted: %w", err)
	}
	id, err := identity.Load(*stateDir)
	if err != nil {
		return fmt.Errorf("cannot load the Agent identity: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The package manager may write its cache and log under HOME (dnf5 does).
	// Without this directory, only reading Available updates on dnf fails.
	if err := os.MkdirAll(*cacheDir, 0o700); err != nil {
		log.Warn("cannot make the cache directory", "error", err)
	}
	run := system.Exec([]string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + *cacheDir})
	var services system.ServiceSource
	if systemd, err := system.ConnectSystemd(ctx); err != nil {
		log.Warn("cannot read systemd over D-Bus, so failed services are not shown", "error", err)
	} else {
		defer systemd.Close()
		services = systemd
	}
	if _, err := os.Stat(*helperSocket); err != nil {
		log.Warn("cannot reach the root helper, so SMART and containers are not shown", "error", err)
	}
	host := system.Detect(ctx, "/", run, system.Statfs, services, helper.Client{Socket: *helperSocket}, time.Now)
	hostname, _ := os.Hostname()
	agent := protocol.AgentInfo{
		Hostname:       hostname,
		AgentVersion:   version.Version,
		Capabilities:   host.Capabilities,
		EnabledActions: []protocol.Action{},
	}
	collector := system.NewCollector(host, system.DefaultIntervals, agent)
	time.Sleep(time.Second) // the first CPU value needs an earlier reading
	state := server.NewState(collector.Sample(ctx))
	collector.Run(ctx, state.Set)

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
			CopiedFrom:     []string{},
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
		Log: log,
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
