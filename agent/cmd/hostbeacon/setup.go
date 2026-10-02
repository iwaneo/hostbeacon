package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// actionNames are the Actions setup asks about, in order, with what each lets
// Home Assistant do.
var actionNames = []struct {
	action protocol.Action
	name   string
	lets   string
}{
	{protocol.ActionReboot, "Reboot", "Home Assistant can restart this Host"},
	{protocol.ActionUpdateRun, "Update run", "Home Assistant can install all Available updates"},
	{protocol.ActionAgentUpdate, "Agent update", "Home Assistant can update the Agent to a newer signed release"},
}

// setup turns Actions on or off, allows Home Assistant's VPN address, sets
// the refresh time, opens the firewall port, and shows a Pairing code (v1
// spec §4.3). Each question
// keeps the current answer on Enter, so after install every Action defaults
// to No. With any flag it asks nothing and changes only what the flags name.
func setup(args []string, o owner) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "the Host config file")
	stateDir := flags.String("state-dir", defaultStateDir, "the Agent's state directory")
	actionsFlag := flags.String("actions", "", "the Actions to turn on, comma-separated: reboot, update_run, agent_update, or none; the others are turned off")
	vpnFlag := flags.String("vpn-address", "", "Home Assistant's single VPN address (for example its Tailscale address), or none")
	refreshFlag := flags.String("refresh-time", "", "the hour of the daily package list refresh in this Host's local time, such as 03:00, or none for once every 24 hours")
	openFirewall := flags.Bool("open-firewall", false, "open the Agent's TCP port in firewalld or ufw")
	pairNow := flags.Bool("pair", false, "show a Pairing code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	current, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	firewall := detectFirewall(ctx, o)

	answers := config.Setup{EnabledActions: current.EnabledActions}
	interactive := true
	flags.Visit(func(f *flag.Flag) {
		if f.Name != "config" && f.Name != "state-dir" {
			interactive = false
		}
	})
	in := bufio.NewReader(o.in)
	if interactive {
		fmt.Fprintln(o.out, "Actions let Home Assistant change this Host. Each one is off unless you turn it on.")
		answers.EnabledActions = nil
		for _, a := range actionNames {
			question := fmt.Sprintf("Turn on %s? %s. [y/N]: ", a.name, a.lets)
			on := slices.Contains(current.EnabledActions, a.action)
			if on {
				question = fmt.Sprintf("%s is on now: %s. Keep it on? [Y/n]: ", a.name, a.lets)
			}
			if ask(in, o, question, on) {
				answers.EnabledActions = append(answers.EnabledActions, a.action)
			}
		}
		if o.tailscale() {
			answers.VPNAddress = askVPNAddress(in, o, current.VPNAddress)
		}
		if current.PackageListRefresh && o.packageManager() {
			answers.RefreshTime = askRefreshTime(in, o, current.RefreshTime)
		}
		*openFirewall = firewall != "" && ask(in, o, fmt.Sprintf("%s is running. Open TCP port %d for the Agent? [y/N]: ", firewall, current.Port), false)
	} else {
		if flagSet(flags, "actions") {
			if answers.EnabledActions, err = parseActions(*actionsFlag); err != nil {
				return err
			}
		}
		if flagSet(flags, "vpn-address") {
			address := *vpnFlag
			if address == "none" {
				address = ""
			} else if _, err := netip.ParseAddr(address); err != nil {
				return fmt.Errorf("--vpn-address must be a single address, or none: %w", err)
			}
			answers.VPNAddress = &address
		}
		if flagSet(flags, "refresh-time") {
			refreshTime := ""
			if *refreshFlag != "none" {
				if refreshTime, err = config.ParseRefreshTime(*refreshFlag); err != nil {
					return fmt.Errorf("--refresh-time must be a whole hour, such as 03:00, or none: %w", err)
				}
			}
			answers.RefreshTime = &refreshTime
		}
	}

	if err := config.WriteSetup(*configPath, answers); err != nil {
		return err
	}
	on := "No Action is turned on."
	if names := actionList(answers.EnabledActions); names != "" {
		on = "Turned on: " + names + "."
	}
	fmt.Fprintf(o.out, "\nSaved %s. %s\n", *configPath, on)
	if answers.RefreshTime != nil {
		fmt.Fprintln(o.out, refreshSchedule(*answers.RefreshTime))
	}
	if err := o.restart(); err != nil {
		fmt.Fprintln(o.out, "Restart the Agent now, so Home Assistant sees the change: sudo systemctl restart hostbeacon")
	}
	if *openFirewall {
		if err := openPort(ctx, o, firewall, current.Port); err != nil {
			return err
		}
	} else if firewall == "" && interactive {
		fmt.Fprintf(o.out, "No firewalld or ufw is running. If another firewall runs, allow TCP port %d from Home Assistant.\n", current.Port)
	}
	if interactive {
		*pairNow = ask(in, o, "\nShow a Pairing code for Home Assistant now? [Y/n]: ", true)
	}
	if *pairNow {
		fmt.Fprintln(o.out)
		return printPairingCode(o.out, *stateDir)
	}
	return nil
}

// flagSet says whether the owner gave the flag name.
func flagSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// ask asks a yes/no question. Enter, or the end of the input, gives def.
func ask(in *bufio.Reader, o owner, question string, def bool) bool {
	for {
		fmt.Fprint(o.out, question)
		line, err := in.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			if err != nil {
				fmt.Fprintln(o.out)
			}
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		if err != nil {
			fmt.Fprintln(o.out)
			return def
		}
		fmt.Fprintln(o.out, "Answer y or n.")
	}
}

// askVPNAddress asks for Home Assistant's single Tailscale address. It never
// offers to allow all of Tailscale's range (v1 spec §4.2).
func askVPNAddress(in *bufio.Reader, o owner, current netip.Addr) *string {
	question := "Tailscale runs on this Host. If Home Assistant reaches it over Tailscale, enter Home Assistant's Tailscale address (for example 100.101.102.103), or press Enter to skip: "
	if current.IsValid() {
		question = fmt.Sprintf("Tailscale runs on this Host. Home Assistant's Tailscale address is %s. Enter a new one, none to remove it, or press Enter to keep it: ", current)
	}
	for {
		fmt.Fprint(o.out, question)
		line, err := in.ReadString('\n')
		text := strings.TrimSpace(line)
		switch {
		case text == "":
			if err != nil {
				fmt.Fprintln(o.out)
			}
			return nil
		case strings.EqualFold(text, "none"):
			empty := ""
			return &empty
		}
		if addr, parseErr := netip.ParseAddr(text); parseErr == nil {
			text = addr.String()
			return &text
		}
		if err != nil {
			fmt.Fprintln(o.out)
			return nil
		}
		fmt.Fprintln(o.out, "Enter one address, such as 100.101.102.103, not a range.")
	}
}

// askRefreshTime asks for the hour of the daily package list refresh (v1
// spec §4.6). Enter keeps the current answer.
func askRefreshTime(in *bufio.Reader, o owner, current string) *string {
	question := "The package list is refreshed once every 24 hours. To refresh it at a fixed hour of this Host's local time, enter the hour (for example 03:00), or press Enter to skip: "
	if current != "" {
		question = fmt.Sprintf("The package list is refreshed daily at %s. Enter a new hour, none for once every 24 hours, or press Enter to keep it: ", current)
	}
	for {
		fmt.Fprint(o.out, question)
		line, err := in.ReadString('\n')
		text := strings.TrimSpace(line)
		switch {
		case text == "":
			if err != nil {
				fmt.Fprintln(o.out)
			}
			return nil
		case strings.EqualFold(text, "none"):
			empty := ""
			return &empty
		}
		if refreshTime, parseErr := config.ParseRefreshTime(text); parseErr == nil {
			return &refreshTime
		}
		if err != nil {
			fmt.Fprintln(o.out)
			return nil
		}
		fmt.Fprintln(o.out, "Enter a whole hour, such as 03:00.")
	}
}

// refreshSchedule says when the package list is refreshed.
func refreshSchedule(refreshTime string) string {
	if refreshTime == "" {
		return "The package list is refreshed once every 24 hours."
	}
	return fmt.Sprintf("The package list is refreshed daily at %s, this Host's local time.", refreshTime)
}

func parseActions(text string) ([]protocol.Action, error) {
	var list []protocol.Action
	if text == "" || text == "none" {
		return list, nil
	}
	for _, name := range strings.Split(text, ",") {
		action := protocol.Action(strings.TrimSpace(name))
		known := false
		for _, a := range actionNames {
			known = known || a.action == action
		}
		if !known {
			return nil, fmt.Errorf("unknown Action %q: use reboot, update_run, agent_update, or none", action)
		}
		if !slices.Contains(list, action) {
			list = append(list, action)
		}
	}
	return list, nil
}

// actionList names the Actions in list, or is empty.
func actionList(list []protocol.Action) string {
	var names []string
	for _, a := range actionNames {
		if slices.Contains(list, a.action) {
			names = append(names, a.name)
		}
	}
	return strings.Join(names, ", ")
}

// detectFirewall names the firewall manager that is running: firewalld, ufw,
// or none.
func detectFirewall(ctx context.Context, o owner) string {
	if out, err := o.run(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(string(out)) == "running" {
		return "firewalld"
	}
	if out, err := o.run(ctx, "ufw", "status"); err == nil && strings.HasPrefix(string(out), "Status: active") {
		return "ufw"
	}
	return ""
}

// openPort opens the Agent's TCP port. The Agent itself still accepts only
// the allowed source addresses.
func openPort(ctx context.Context, o owner, firewall string, port int) error {
	rule := strconv.Itoa(port) + "/tcp"
	var commands [][]string
	switch firewall {
	case "firewalld":
		commands = [][]string{{"firewall-cmd", "--permanent", "--add-port=" + rule}, {"firewall-cmd", "--reload"}}
	case "ufw":
		commands = [][]string{{"ufw", "allow", rule}}
	default:
		fmt.Fprintf(o.out, "No firewalld or ufw is running, so no port was opened. If another firewall runs, allow TCP port %d from Home Assistant.\n", port)
		return nil
	}
	for _, args := range commands {
		if _, err := o.run(ctx, args[0], args[1:]...); err != nil {
			return errors.Join(fmt.Errorf("cannot open TCP port %d in %s", port, firewall), err)
		}
	}
	fmt.Fprintf(o.out, "Opened TCP port %d in %s.\n", port, firewall)
	return nil
}
