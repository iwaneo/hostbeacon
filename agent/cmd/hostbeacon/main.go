// Command hostbeacon is the Hostbeacon Agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
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

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", config.DefaultPath, "the Host config file")
	stateDir := flags.String("state-dir", defaultStateDir, "the Agent's state directory")
	cacheDir := flags.String("cache-dir", defaultCacheDir, "a directory the package manager may use for its cache")
	flags.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

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
	os.MkdirAll(*cacheDir, 0o700)
	run := system.Exec([]string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + *cacheDir})
	var services system.ServiceSource
	if systemd, err := system.ConnectSystemd(ctx); err != nil {
		log.Warn("cannot read systemd over D-Bus, so failed services are not shown", "error", err)
	} else {
		defer systemd.Close()
		services = systemd
	}
	host := system.Detect(ctx, "/", run, system.Statfs, services, time.Now)
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

	s := &server.Server{
		Config:   hostConfig,
		Identity: id,
		Pairings: pairing.Open(*stateDir, time.Now),
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
	return s.Serve(ctx, listener)
}

// optional is null for empty text.
func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}
