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

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/version"
)

const usage = `Usage:
  hostbeacon-helper serve    run the root helper (systemd starts it as root)
  hostbeacon-helper version  show the version
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
	server := &helper.Server{
		AllowedUID: uid,
		LoadConfig: loadConfig,
		Jobs:       helper.HostJobs{Root: "/", Run: command.Exec(env), Stream: command.ExecStream(env)},
		Log:        log,
	}
	log.Info("listening", "socket", *socket, "user", *agentUser)
	return server.Serve(ctx, listener)
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
