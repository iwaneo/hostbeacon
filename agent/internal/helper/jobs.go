package helper

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/iwaneo/hostbeacon/agent/internal/command"
)

// HostJobs does the jobs on the real Host. Root is "/" except in tests.
type HostJobs struct {
	Root   string
	Run    command.Command
	Stream command.Stream
}

var (
	// devicePattern is a plain block device name such as sda or nvme0n1.
	devicePattern = regexp.MustCompile(`^[a-z]+[0-9a-z]*$`)
	// virtualDevices are block devices that are not physical disks. NVMe
	// multipath paths (nvme0c0n1) are left out as well; their namespace
	// (nvme0n1) is read instead.
	virtualDevices = regexp.MustCompile(`^(loop|ram|zram|dm-|md|sr|fd|nbd|rbd|drbd|zd|bcache|mmcblk|nullb|pmem)|^nvme[0-9]+c[0-9]+n`)
	// virtualModels are disks a hypervisor emulates; their SMART is made up.
	virtualModels = regexp.MustCompile(`(?i)^(QEMU|VBOX|VMware|Virtual disk|Msft Virtual)`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ReadSmart reads SMART from each physical disk with smartctl. It never
// wakes a disk: smartctl checks the power mode first and skips a disk in
// standby. Disks without SMART (virtual disks in a VM) are left out. Without
// smartctl there are no disks.
func (h HostJobs) ReadSmart(ctx context.Context) ([]SmartDisk, error) {
	entries, err := os.ReadDir(filepath.Join(h.Root, "sys", "block"))
	if err != nil {
		return []SmartDisk{}, nil
	}
	disks := []SmartDisk{}
	for _, entry := range entries {
		name := entry.Name()
		// Names come from the Host itself, and are checked before use.
		if !devicePattern.MatchString(name) || virtualDevices.MatchString(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(h.Root, "sys", "block", name, "device")); err != nil {
			continue
		}
		out, err := h.Run(ctx, "smartctl", "--json=c", "--nocheck=standby", "--info", "--health", "--attributes", "--log=devstat", "/dev/"+name)
		if errors.Is(err, exec.ErrNotFound) {
			return []SmartDisk{}, nil
		}
		// smartctl sets exit status bits for disk problems, and still
		// prints the data, so the output is read whatever the status.
		if disk, ok := parseSmartctl(name, out); ok {
			disks = append(disks, disk)
		}
	}
	return disks, nil
}

type smartctlOutput struct {
	Smartctl struct {
		Messages []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	Device struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	ScsiModel    string `json:"scsi_model_name"`
	RotationRate *int   `json:"rotation_rate"`
	SmartSupport *struct {
		Available bool `json:"available"`
	} `json:"smart_support"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
	EnduranceUsed *struct {
		CurrentPercent *float64 `json:"current_percent"`
	} `json:"endurance_used"`
	NVMeLog *struct {
		PercentageUsed *float64 `json:"percentage_used"`
	} `json:"nvme_smart_health_information_log"`
	DeviceStatistics *struct {
		Pages []struct {
			Table []struct {
				Name  string   `json:"name"`
				Value *float64 `json:"value"`
			} `json:"table"`
		} `json:"pages"`
	} `json:"ata_device_statistics"`
}

// parseSmartctl reads smartctl's JSON for one disk. ok is false when the
// disk has no SMART.
func parseSmartctl(device string, out []byte) (disk SmartDisk, ok bool) {
	var s smartctlOutput
	if err := json.Unmarshal(out, &s); err != nil {
		return SmartDisk{}, false
	}
	disk.Device = device
	if s.SmartStatus == nil {
		// "Device is in STANDBY mode, exit(2)": skipped, so not woken.
		for _, message := range s.Smartctl.Messages {
			if strings.Contains(message.String, " mode, exit(") {
				disk.Standby = true
				return disk, true
			}
		}
	}
	if (s.SmartSupport == nil || !s.SmartSupport.Available) && s.Device.Protocol != "NVMe" {
		return SmartDisk{}, false
	}
	if virtualModels.MatchString(s.ModelName) || virtualModels.MatchString(s.ScsiModel) {
		return SmartDisk{}, false
	}
	if s.SmartStatus != nil {
		health := "failing"
		if s.SmartStatus.Passed {
			health = "ok"
		}
		disk.Health = &health
	}
	if s.Temperature != nil {
		disk.TemperatureCelsius = s.Temperature.Current
	}
	if s.RotationRate == nil || *s.RotationRate == 0 {
		disk.WearPercent = s.wear()
	}
	return disk, true
}

// wear is the part of an SSD's rated endurance that is used. It can pass
// 100 on a worn disk; the protocol stops at 100.
func (s smartctlOutput) wear() *float64 {
	var wear *float64
	switch {
	case s.EnduranceUsed != nil && s.EnduranceUsed.CurrentPercent != nil:
		wear = s.EnduranceUsed.CurrentPercent
	case s.NVMeLog != nil && s.NVMeLog.PercentageUsed != nil:
		wear = s.NVMeLog.PercentageUsed
	case s.DeviceStatistics != nil:
		for _, page := range s.DeviceStatistics.Pages {
			for _, row := range page.Table {
				if row.Name == "Percentage Used Endurance Indicator" && row.Value != nil {
					wear = row.Value
				}
			}
		}
	}
	if wear != nil {
		capped := min(max(*wear, 0), 100)
		return &capped
	}
	return nil
}

var engines = []string{"docker", "podman"}

// ReadContainers lists the containers of Docker and Podman (root's
// containers; rootless Podman containers of other users are not seen).
func (h HostJobs) ReadContainers(ctx context.Context) (Containers, error) {
	result := Containers{Engines: []string{}, Items: []Container{}}
	seen := map[string]bool{}
	for _, engine := range engines {
		out, err := h.Run(ctx, engine, "ps", "--all", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}\t{{.State}}\t{{.Status}}")
		if errors.Is(err, exec.ErrNotFound) {
			continue
		}
		result.Engines = append(result.Engines, engine)
		if err != nil {
			// For example, the Docker daemon is stopped. Counts without
			// this engine would be wrong.
			result.Error = fmt.Sprintf("%s cannot list its containers: %v", engine, err)
			continue
		}
		for line := range strings.Lines(string(out)) {
			fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
			// Docker behind a podman-docker shim lists the same containers.
			if len(fields) != 4 || seen[fields[0]] {
				continue
			}
			seen[fields[0]] = true
			name, _, _ := strings.Cut(fields[1], ",")
			result.Items = append(result.Items, Container{Name: name, State: containerState(fields[2], fields[3])})
		}
	}
	if result.Error != "" {
		result.Items = []Container{}
	}
	slices.SortFunc(result.Items, func(a, b Container) int { return cmp.Compare(a.Name, b.Name) })
	return result, nil
}

// containerState maps an engine's state to running, stopped, or unhealthy.
// A container that keeps restarting counts as unhealthy.
func containerState(state, status string) string {
	switch {
	case state == "running" && strings.Contains(status, "(unhealthy)"), state == "restarting":
		return StateUnhealthy
	case state == "running":
		return StateRunning
	default:
		return StateStopped
	}
}

// WatchContainers follows the event streams of Docker and Podman. Commands
// run inside a container (health checks run one every few seconds) are not
// changes.
func (h HostJobs) WatchContainers(ctx context.Context, changed func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	formats := map[string]string{"docker": "{{.Action}}", "podman": "{{.Status}}"}
	errs := make(chan error, len(engines))
	var mu sync.Mutex
	for _, engine := range engines {
		go func() {
			errs <- h.Stream(ctx, func(line string) {
				if strings.HasPrefix(line, "exec") {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				changed()
			}, engine, "events", "--filter", "type=container", "--format", formats[engine])
		}()
	}
	missing := 0
	for range engines {
		err := <-errs
		if errors.Is(err, exec.ErrNotFound) {
			missing++
			continue
		}
		// One stream ended: stop the others, and the caller starts again.
		if err == nil {
			err = errors.New("the container event stream ended")
		}
		return err
	}
	if missing == len(engines) {
		return errors.New("no container engine is installed")
	}
	return ctx.Err()
}

// ReadSMBIOSUUID reads the SMBIOS UUID, which only root can read. It is nil
// when the Host has none.
func (h HostJobs) ReadSMBIOSUUID(context.Context) (*string, error) {
	data, err := os.ReadFile(filepath.Join(h.Root, "sys", "class", "dmi", "id", "product_uuid"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uuid := strings.ToLower(strings.TrimSpace(string(data)))
	if !uuidPattern.MatchString(uuid) {
		return nil, nil
	}
	return &uuid, nil
}
