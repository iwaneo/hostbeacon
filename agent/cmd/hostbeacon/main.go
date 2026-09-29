// Command hostbeacon is the Hostbeacon Agent.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
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
	systemInterval  = 30 * time.Second
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

	sampler := system.NewSampler("/")
	sampler.Sample() // the first CPU value needs an earlier reading
	time.Sleep(time.Second)
	state := server.NewState(sampler.Sample())
	go func() {
		ticker := time.NewTicker(systemInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state.SetSystem(sampler.Sample())
			}
		}
	}()

	hostname, _ := os.Hostname()
	s := &server.Server{
		Config:   hostConfig,
		Identity: id,
		Pairings: pairing.Open(*stateDir, time.Now),
		State:    state,
		Hello: protocol.HelloRequest{
			InstanceID:     id.InstanceID,
			RunID:          identity.NewUUID(),
			CopiedFrom:     []string{},
			Hostname:       hostname,
			AgentVersion:   version.Version,
			Capabilities:   []string{},
			EnabledActions: []protocol.Action{},
			Distro:         readDistro("/etc/os-release"),
			Architecture:   runtime.GOARCH,
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

// readDistro reads ID, NAME, and VERSION_ID from os-release. A missing value
// is null.
func readDistro(path string) protocol.Distro {
	values := map[string]*string{}
	if file, err := os.Open(path); err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if name, value, ok := strings.Cut(scanner.Text(), "="); ok {
				value = strings.Trim(value, `"'`)
				values[name] = &value
			}
		}
	}
	return protocol.Distro{ID: values["ID"], Name: values["NAME"], Version: values["VERSION_ID"]}
}
