package system

import (
	"context"
	"path/filepath"
	"slices"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// Capabilities in hello (v1 spec §4.5). A missing one means "not supported
// here", and Home Assistant makes no entity for it.
const (
	CapabilityLoad             = "load"
	CapabilityTemperatures     = "temperatures"
	CapabilityDisks            = "disks"
	CapabilityNetwork          = "network"
	CapabilityFailedServices   = "failed_services"
	CapabilityAvailableUpdates = "available_updates"
)

// Intervals says how often each group is read (v1 spec §4.6).
type Intervals struct {
	System, Network, Disks, Temperatures, RebootRequired, AvailableUpdates time.Duration
	// Failed services: at most one read per ServicesGap after a change
	// signal, and a full check every ServicesFull.
	ServicesGap, ServicesFull time.Duration
}

// DefaultIntervals are the v1 intervals.
var DefaultIntervals = Intervals{
	System:           30 * time.Second,
	Network:          30 * time.Second,
	Disks:            60 * time.Second,
	Temperatures:     60 * time.Second,
	RebootRequired:   5 * time.Minute,
	AvailableUpdates: 15 * time.Minute,
	ServicesGap:      10 * time.Second,
	ServicesFull:     60 * time.Second,
}

// Host is what the Agent found on this Host at start: its environment and
// the data sources that work here.
type Host struct {
	Root         string
	Run          Command
	Statfs       func(path string) (FSSize, error)
	Now          func() time.Time
	Environment  *string
	Container    bool
	Release      map[string]string
	Kernel       *string
	Capabilities []string

	services       ServiceSource
	cpu            CPUSensor
	packageManager string
	lastBoot       *string
}

// Detect finds the environment and every data source. services is nil
// when systemd cannot be reached over D-Bus. In a container, load and CPU
// temperature show the hypervisor's values, so they are left out.
func Detect(ctx context.Context, root string, run Command, statfs func(string) (FSSize, error), services ServiceSource, now func() time.Time) *Host {
	h := &Host{Root: root, Run: run, Statfs: statfs, Now: now, Release: ReadOSRelease(root), Kernel: Kernel(root), Capabilities: []string{}, services: services}
	h.Environment, h.Container = DetectEnvironment(ctx, run)
	h.lastBoot = LastBoot(root, h.Container, now())
	if !h.Container && exists(filepath.Join(root, "proc", "loadavg")) {
		h.Capabilities = append(h.Capabilities, CapabilityLoad)
	}
	if cpu, ok := FindCPUTemperature(root); ok && !h.Container {
		h.cpu = cpu
		h.Capabilities = append(h.Capabilities, CapabilityTemperatures)
	}
	if exists(filepath.Join(root, "proc", "self", "mountinfo")) {
		h.Capabilities = append(h.Capabilities, CapabilityDisks)
	}
	if exists(filepath.Join(root, "sys", "class", "net")) {
		h.Capabilities = append(h.Capabilities, CapabilityNetwork)
	}
	if services != nil {
		h.Capabilities = append(h.Capabilities, CapabilityFailedServices)
	}
	if h.packageManager = DetectPackageManager(root); h.packageManager != "" {
		h.Capabilities = append(h.Capabilities, CapabilityAvailableUpdates)
	}
	return h
}

func (h *Host) has(capability string) bool {
	return slices.Contains(h.Capabilities, capability)
}

// Collector reads each group on its interval and publishes it. Publish
// must ignore values that did not change, so no delta is sent for them.
type Collector struct {
	host      *Host
	intervals Intervals
	agent     protocol.AgentInfo
	system    *Sampler
	network   *NetworkSampler
	disks     *DiskSampler
}

// NewCollector makes a Collector and takes the first CPU and network
// readings, so the next Sample can give usage and rates. agent is the agent
// group; its hostname is re-read with the system group.
func NewCollector(host *Host, intervals Intervals, agent protocol.AgentInfo) *Collector {
	c := &Collector{
		host:      host,
		intervals: intervals,
		agent:     agent,
		system:    NewSampler(host.Root, host.has(CapabilityLoad)),
		network:   NewNetworkSampler(host.Root, host.Container, host.Now),
		disks:     NewDiskSampler(host.Root, host.Statfs),
	}
	c.system.Sample()
	c.network.Sample()
	return c
}

// Sample reads every group once, for the first snapshot. Groups for
// missing capabilities are left out.
func (c *Collector) Sample(ctx context.Context) protocol.Groups {
	groups := protocol.Groups{Agent: c.agentGroup(), System: ptr(c.system.Sample()), Flags: c.flags()}
	if c.host.has(CapabilityDisks) {
		groups.Disks = ptr(c.disks.Sample())
	}
	if c.host.has(CapabilityNetwork) {
		groups.Network = ptr(c.network.Sample())
	}
	if c.host.has(CapabilityTemperatures) {
		groups.Temperatures = ptr(c.host.cpu.Read())
	}
	if c.host.has(CapabilityAvailableUpdates) {
		groups.AvailableUpdates = c.availableUpdates(ctx)
	}
	if c.host.has(CapabilityFailedServices) {
		names, err := c.host.services.Failed(ctx)
		groups.FailedServices = ptr(nameList(names, err))
	}
	return groups
}

// Run reads each group on its interval until ctx ends.
func (c *Collector) Run(ctx context.Context, publish func(protocol.Groups)) {
	every := func(interval time.Duration, read func() protocol.Groups) {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					publish(read())
				}
			}
		}()
	}
	every(c.intervals.System, func() protocol.Groups {
		return protocol.Groups{Agent: c.agentGroup(), System: ptr(c.system.Sample())}
	})
	every(c.intervals.RebootRequired, func() protocol.Groups { return protocol.Groups{Flags: c.flags()} })
	if c.host.has(CapabilityNetwork) {
		every(c.intervals.Network, func() protocol.Groups { return protocol.Groups{Network: ptr(c.network.Sample())} })
	}
	if c.host.has(CapabilityDisks) {
		every(c.intervals.Disks, func() protocol.Groups { return protocol.Groups{Disks: ptr(c.disks.Sample())} })
	}
	if c.host.has(CapabilityTemperatures) {
		every(c.intervals.Temperatures, func() protocol.Groups { return protocol.Groups{Temperatures: ptr(c.host.cpu.Read())} })
	}
	if c.host.has(CapabilityAvailableUpdates) {
		every(c.intervals.AvailableUpdates, func() protocol.Groups { return protocol.Groups{AvailableUpdates: c.availableUpdates(ctx)} })
	}
	if c.host.has(CapabilityFailedServices) {
		go WatchFailedServices(ctx, c.host.services, c.intervals.ServicesGap, c.intervals.ServicesFull, func(list protocol.NameList) {
			publish(protocol.Groups{FailedServices: &list})
		})
	}
}

func (c *Collector) agentGroup() *protocol.AgentInfo {
	agent := c.agent
	if hostname := Hostname(c.host.Root); hostname != "" {
		agent.Hostname = hostname
	}
	return &agent
}

// flags holds Reboot required and the last boot. The package task and
// package system flags are not read yet.
func (c *Collector) flags() *protocol.Flags {
	return &protocol.Flags{
		RebootRequired: RebootRequired(c.host.Root, c.host.Container, c.host.Release),
		LastBoot:       c.host.lastBoot,
	}
}

// availableUpdates is unknown (count null) when the cache cannot be read.
func (c *Collector) availableUpdates(ctx context.Context) *protocol.AvailableUpdates {
	updates, err := ReadAvailableUpdates(ctx, c.host.Run, c.host.packageManager)
	if err != nil {
		return &protocol.AvailableUpdates{Packages: []protocol.Package{}}
	}
	return &updates
}

func ptr[T any](value T) *T { return &value }
