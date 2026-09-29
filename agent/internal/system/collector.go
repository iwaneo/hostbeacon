package system

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/helper"
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
	CapabilitySmart            = "smart"
	CapabilityContainers       = "containers"
)

// Intervals says how often each group is read (v1 spec §4.6).
type Intervals struct {
	System, Network, Disks, Temperatures, RebootRequired, AvailableUpdates time.Duration
	// Failed services: at most one read per ServicesGap after a change
	// signal, and a full check every ServicesFull.
	ServicesGap, ServicesFull time.Duration
	// SMART is read every Smart. Containers are read like failed services;
	// when the event stream fails, it starts again after ContainersRetry.
	Smart                                          time.Duration
	ContainersGap, ContainersFull, ContainersRetry time.Duration
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
	Smart:            time.Hour,
	ContainersGap:    10 * time.Second,
	ContainersFull:   60 * time.Second,
	ContainersRetry:  30 * time.Second,
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
	// Tasks is nil when systemd cannot be reached over D-Bus.
	Tasks PackageTasks

	services       ServiceSource
	helper         RootHelper
	smartDisks     []helper.SmartDisk // the first SMART read
	cpu            CPUTemperature
	packageManager string
	lastBoot       *string
}

// Detect finds the environment and every data source. services is nil
// when systemd cannot be reached over D-Bus; rootHelper is nil without the
// root helper. In a container, load, CPU temperature, and SMART show the
// hypervisor's values, so they are left out.
func Detect(ctx context.Context, root string, run Command, statfs func(string) (FSSize, error), services ServiceSource, rootHelper RootHelper, now func() time.Time) *Host {
	h := &Host{Root: root, Run: run, Statfs: statfs, Now: now, Release: ReadOSRelease(root), Kernel: Kernel(root), Capabilities: []string{}, services: services, helper: rootHelper}
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
	if h.packageManager = helper.DetectPackageManager(root); h.packageManager != "" {
		h.Capabilities = append(h.Capabilities, CapabilityAvailableUpdates)
	}
	if rootHelper != nil {
		// SMART only with physical disks; a VM's virtual disks have none.
		if disks, err := rootHelper.ReadSmart(ctx); err == nil && len(disks) > 0 && !h.Container {
			h.smartDisks = disks
			h.Capabilities = append(h.Capabilities, CapabilitySmart)
		}
		if containers, err := rootHelper.ReadContainers(ctx); err == nil && len(containers.Engines) > 0 {
			h.Capabilities = append(h.Capabilities, CapabilityContainers)
		}
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
	smart     *SmartSampler
	// flagsMu makes each read and publish of the flags one step, so an
	// older read is never published after a newer one.
	flagsMu sync.Mutex
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
		smart:     &SmartSampler{helper: host.helper},
	}
	c.system.Sample()
	c.network.Sample()
	return c
}

// Sample reads every group once, for the first snapshot. Groups for
// missing capabilities are left out.
func (c *Collector) Sample(ctx context.Context) protocol.Groups {
	groups := protocol.Groups{Agent: c.agentGroup(), System: ptr(c.system.Sample()), Flags: c.flags(ctx)}
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
	if c.host.has(CapabilitySmart) {
		groups.Smart = c.smart.group(c.host.smartDisks)
	}
	if c.host.has(CapabilityContainers) {
		containers, err := c.host.helper.ReadContainers(ctx)
		groups.Containers = ptr(containersGroup(containers, err))
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
	go func() {
		ticker := time.NewTicker(c.intervals.RebootRequired)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.publishFlags(ctx, publish)
			}
		}
	}()
	if c.host.Tasks != nil {
		go c.watchPackageTasks(ctx, publish)
	}
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
	if c.host.has(CapabilitySmart) {
		every(c.intervals.Smart, func() protocol.Groups { return protocol.Groups{Smart: c.smart.Sample(ctx)} })
	}
	if c.host.has(CapabilityContainers) {
		go WatchContainers(ctx, c.host.helper, c.intervals.ContainersGap, c.intervals.ContainersFull, c.intervals.ContainersRetry, func(group protocol.Containers) {
			publish(protocol.Groups{Containers: &group})
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

// publishFlags reads and publishes the flags group.
func (c *Collector) publishFlags(ctx context.Context, publish func(protocol.Groups)) *protocol.Flags {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	flags := c.flags(ctx)
	publish(protocol.Groups{Flags: flags})
	return flags
}

// flags holds Reboot required, the last boot, and whether a package task
// runs. The package system flags are not read yet.
func (c *Collector) flags(ctx context.Context) *protocol.Flags {
	flags := &protocol.Flags{
		RebootRequired: RebootRequired(c.host.Root, c.host.Container),
		LastBoot:       c.host.lastBoot,
	}
	if c.host.Tasks != nil {
		// Unknown counts as not running: the flag has no unknown value.
		flags.PackageTaskRunning, _ = c.host.Tasks.PackageTaskRunning(ctx)
	}
	return flags
}

// availableUpdates is unknown (count null) when the cache cannot be read.
func (c *Collector) availableUpdates(ctx context.Context) *protocol.AvailableUpdates {
	updates, err := ReadAvailableUpdates(ctx, c.host.Run, c.host.packageManager)
	if err != nil {
		updates = protocol.AvailableUpdates{Packages: []protocol.Package{}}
	}
	updates.LastRefresh = c.lastRefresh()
	return &updates
}

func ptr[T any](value T) *T { return &value }
