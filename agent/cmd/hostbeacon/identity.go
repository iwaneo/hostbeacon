package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
	"github.com/iwaneo/hostbeacon/agent/internal/config"
	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/identity"
	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
)

// owner is what the owner commands read from and act on.
type owner struct {
	in  io.Reader
	out io.Writer
	// signals reads the identity signals. Owner commands run as root, so
	// they read the SMBIOS UUID themselves.
	signals func() identity.Signals
	// restart restarts the running Agent, so it uses the new identity or
	// Host config.
	restart func() error
	// run runs a program, such as systemctl or firewall-cmd.
	run command.Command
	// tailscale says whether Tailscale is on this Host.
	tailscale func() bool
	// packageManager says whether apt or dnf is on this Host, so the
	// package list refresh can run.
	packageManager func() bool
}

func systemOwner() owner {
	return owner{
		in:  os.Stdin,
		out: os.Stdout,
		signals: func() identity.Signals {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return readSignals(ctx, "/", helper.HostJobs{Root: "/"}.ReadSMBIOSUUID)
		},
		restart: func() error { return exec.Command("systemctl", "try-restart", "hostbeacon.service").Run() },
		run:     command.Exec([]string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}),
		tailscale: func() bool {
			_, err := exec.LookPath("tailscale")
			_, statErr := os.Stat("/sys/class/net/tailscale0")
			return err == nil || statErr == nil
		},
		packageManager: func() bool { return helper.DetectPackageManager("/") != "" },
	}
}

// readSignals reads the signals of the clone check under root: the machine
// ID, and the SMBIOS UUID through readSMBIOS.
func readSignals(ctx context.Context, root string, readSMBIOS func(context.Context) (*string, error)) identity.Signals {
	signals := identity.Signals{Values: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(root, "etc", "machine-id"))
	switch id := strings.ToLower(strings.TrimSpace(string(data))); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		signals.Unreadable = append(signals.Unreadable, identity.SignalMachineID)
	case id != "" && id != "uninitialized":
		signals.Values[identity.SignalMachineID] = id
	}
	uuid, err := readSMBIOS(ctx)
	switch {
	case err != nil:
		signals.Unreadable = append(signals.Unreadable, identity.SignalSMBIOSUUID)
	case uuid != nil:
		signals.Values[identity.SignalSMBIOSUUID] = *uuid
	}
	return signals
}

// signalNames names signals for the owner.
func signalNames(names []string) string {
	text := map[string]string{
		identity.SignalMachineID:  "the machine ID (/etc/machine-id)",
		identity.SignalSMBIOSUUID: "the SMBIOS UUID",
	}
	var parts []string
	for _, name := range names {
		if t, ok := text[name]; ok {
			parts = append(parts, t)
		} else {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, " and ")
}

// holdAdvice says why the Agent is in identity hold and what to run.
func holdAdvice(missing []string) string {
	advice := fmt.Sprintf(`The Agent is in identity hold: it accepts no connections, because it cannot
read %s, which it read at install.
`, signalNames(missing))
	if slices.Contains(missing, identity.SignalSMBIOSUUID) {
		advice += `The Agent reads the SMBIOS UUID through the root helper. If the helper is
not running, start it and restart the Agent:
  sudo systemctl restart hostbeacon-helper hostbeacon
`
	}
	return advice + `Otherwise:
  If this is the same machine:               sudo hostbeacon keep-identity
  If the signal is gone for good:            sudo hostbeacon keep-identity --drop-missing
  If this machine is a copy of another Host: sudo hostbeacon reset-identity
`
}

const bestEffort = `Clone detection is best effort. If the Agent does not see that it is a copy,
and Home Assistant does not reach both machines at once, Home Assistant cannot
tell them apart. Run "sudo hostbeacon reset-identity" on every copy of a Host
(for example a cloned VM) before it goes online.
`

// ownerFlags parses the flags every identity command has.
func ownerFlags(name string, args []string, more func(*flag.FlagSet)) (stateDir, actionLog string, err error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.StringVar(&stateDir, "state-dir", defaultStateDir, "the Agent's state directory")
	flags.StringVar(&actionLog, "action-log", helper.DefaultActionLogDir, "the Action log directory")
	if more != nil {
		more(flags)
	}
	if err := flags.Parse(args); err != nil {
		return "", "", err
	}
	if flags.NArg() > 0 {
		return "", "", fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return stateDir, actionLog, nil
}

// confirm asks the owner to type yes, unless yes is set already.
func (o owner) confirm(yes bool) error {
	if yes {
		return nil
	}
	fmt.Fprint(o.out, "Type yes to go on: ")
	line, _ := bufio.NewReader(o.in).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return errors.New("nothing was changed")
	}
	return nil
}

// restartAgent restarts the Agent, or tells the owner to.
func (o owner) restartAgent() {
	if err := o.restart(); err != nil {
		fmt.Fprintln(o.out, "Restart the Agent now: sudo systemctl restart hostbeacon")
		return
	}
	fmt.Fprintln(o.out, "The Agent was restarted, if it was running.")
}

// resetIdentity gives the Agent a new identity by hand, as for a copy.
func resetIdentity(args []string, o owner) error {
	var yes bool
	stateDir, actionLog, err := ownerFlags("reset-identity", args, func(flags *flag.FlagSet) {
		flags.BoolVar(&yes, "yes", false, "do not ask")
	})
	if err != nil {
		return err
	}
	fmt.Fprint(o.out, `reset-identity gives this Agent a new identity, as for a new Host: a new
instance ID, key, and certificate, and no Pairings. Home Assistant no longer
reaches this Host as the Host it shows now; add it again as a new Host.

`+bestEffort+"\n")
	if err := o.confirm(yes); err != nil {
		return err
	}
	before, _ := identity.ReadStatus(stateDir)
	id, err := identity.Reset(stateDir, o.signals())
	if err != nil {
		return err
	}
	fmt.Fprintf(o.out, "New instance ID: %s. Every Pairing was removed.\n", id.InstanceID)
	log := helper.ActionLog{Dir: actionLog, Now: time.Now}
	if err := log.LogIdentityCopy(helper.IdentityCopy{InstanceID: id.InstanceID, CopiedFrom: id.CopiedFrom}); err != nil {
		fmt.Fprintf(o.out, "Warning: cannot write the Action log: %v\n", err)
	}
	if before.InstanceID != "" {
		fmt.Fprintf(o.out, "The old instance ID %s is in the copied-from list.\n", before.InstanceID)
	}
	o.restartAgent()
	return nil
}

// keepIdentity stores the signals of now and ends an identity hold.
func keepIdentity(args []string, o owner) error {
	var dropMissing bool
	stateDir, _, err := ownerFlags("keep-identity", args, func(flags *flag.FlagSet) {
		flags.BoolVar(&dropMissing, "drop-missing", false, "stop checking signals that cannot be read")
	})
	if err != nil {
		return err
	}
	dropped, err := identity.Keep(stateDir, o.signals(), dropMissing)
	var missing *identity.MissingError
	if errors.As(err, &missing) {
		return fmt.Errorf("cannot read %s. If it is gone for good, run: sudo hostbeacon keep-identity --drop-missing", signalNames(missing.Names))
	}
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		fmt.Fprintf(o.out, "Warning: the Agent no longer checks %s. Clone detection is weaker now.\n", signalNames(dropped))
	}
	fmt.Fprintln(o.out, "This Host keeps its identity.")
	o.restartAgent()
	return nil
}

// regenerateKey lists the Pairings, then makes a new key and certificate and
// removes every Pairing (v1 spec §5: for a leaked Agent private key).
func regenerateKey(args []string, o owner) error {
	var yes bool
	stateDir, _, err := ownerFlags("regenerate-key", args, func(flags *flag.FlagSet) {
		flags.BoolVar(&yes, "yes", false, "do not ask")
	})
	if err != nil {
		return err
	}
	list, err := pairing.Open(stateDir, time.Now).List()
	if err != nil {
		return err
	}
	fmt.Fprintln(o.out, "regenerate-key makes a new key and certificate and removes these Pairings:")
	if len(list) == 0 {
		fmt.Fprintln(o.out, "  (none)")
	}
	table := tabwriter.NewWriter(o.out, 0, 0, 2, ' ', 0)
	for _, p := range list {
		fmt.Fprintf(table, "  %s\t%s\n", p.ID, p.Name)
	}
	table.Flush()
	fmt.Fprint(o.out, "Each of these Home Assistants must Re-pair this Host.\n\n")
	if err := o.confirm(yes); err != nil {
		return err
	}
	id, err := identity.RegenerateKey(stateDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Removed every Pairing. New certificate fingerprint: %x\n", id.Fingerprint)
	o.restartAgent()
	return nil
}

// status shows the Agent's identity and Pairings, and what to run.
func status(args []string, o owner) error {
	var configPath string
	stateDir, _, err := ownerFlags("status", args, func(flags *flag.FlagSet) {
		flags.StringVar(&configPath, "config", config.DefaultPath, "the Host config file")
	})
	if err != nil {
		return err
	}
	running := "not running (see: sudo systemctl status hostbeacon)"
	if out, err := o.run(context.Background(), "systemctl", "is-active", "hostbeacon.service"); err == nil && strings.TrimSpace(string(out)) == "active" {
		running = "running"
	}
	fmt.Fprintf(o.out, "Agent:       %s\n", running)
	if c, err := config.Load(configPath); err != nil {
		fmt.Fprintf(o.out, "Host config: cannot read it, so the Agent accepts no connections: %v\n", err)
	} else {
		actions := actionList(c.EnabledActions)
		if actions == "" {
			actions = "none (turn them on with: sudo hostbeacon setup)"
		}
		fmt.Fprintf(o.out, "Actions:     %s\n", actions)
		fmt.Fprintf(o.out, "Port:        %d\n", c.Port)
		if c.VPNAddress.IsValid() {
			fmt.Fprintf(o.out, "VPN address: %s\n", c.VPNAddress)
		}
	}
	id, err := identity.ReadStatus(stateDir)
	if err != nil {
		return err
	}
	if id.InstanceID == "" {
		fmt.Fprintln(o.out, "The Agent has not started yet.")
		return nil
	}
	fmt.Fprintf(o.out, "Instance ID: %s\n", id.InstanceID)
	if len(id.CopiedFrom) > 0 {
		fmt.Fprintf(o.out, "Copied from: %s\n", strings.Join(id.CopiedFrom, ", "))
	}
	if len(id.Hold) > 0 {
		fmt.Fprint(o.out, "\n"+holdAdvice(id.Hold)+"\n")
	} else {
		fmt.Fprintln(o.out, "Identity:    OK")
	}
	list, err := pairing.Open(stateDir, time.Now).List()
	if err != nil {
		return err
	}
	fmt.Fprintf(o.out, "Pairings:    %d (sudo hostbeacon pairings list)\n", len(list))
	return nil
}
