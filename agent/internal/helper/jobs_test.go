package helper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The smartctl outputs below are cut from real smartctl 7.x JSON; serial
// numbers and other unused fields are left out.
const (
	nvmeJSON = `{"smartctl":{"exit_status":0},"device":{"name":"/dev/nvme0n1","type":"nvme","protocol":"NVMe"},
		"smart_support":{"available":true,"enabled":true},"smart_status":{"passed":true,"nvme":{"value":0}},
		"temperature":{"current":39},"endurance_used":{"current_percent":14},
		"nvme_smart_health_information_log":{"temperature":39,"percentage_used":14}}`
	// Older smartctl: no endurance_used, only the NVMe log.
	nvmeOldJSON = `{"smartctl":{"exit_status":0},"device":{"name":"/dev/nvme1n1","type":"nvme","protocol":"NVMe"},
		"smart_status":{"passed":true},"temperature":{"current":41.4},
		"nvme_smart_health_information_log":{"percentage_used":3}}`
	ataSSDJSON = `{"smartctl":{"exit_status":0},"device":{"name":"/dev/sda","type":"sat","protocol":"ATA"},
		"rotation_rate":0,"smart_support":{"available":true,"enabled":true},"smart_status":{"passed":true},
		"temperature":{"current":31},
		"ata_device_statistics":{"pages":[{"number":7,"name":"Solid State Device Statistics","table":[
			{"offset":8,"name":"Percentage Used Endurance Indicator","size":1,"value":7}]}]}}`
	// A failing HDD: exit status bit 3 is set.
	ataHDDFailingJSON = `{"smartctl":{"exit_status":8},"device":{"name":"/dev/sdb","type":"sat","protocol":"ATA"},
		"rotation_rate":7200,"smart_support":{"available":true,"enabled":true},"smart_status":{"passed":false},
		"temperature":{"current":44},"endurance_used":{"current_percent":0}}`
	ataStandbyJSON = `{"smartctl":{"exit_status":2,"messages":[{"string":"Device is in STANDBY mode, exit(2)","severity":"information"}]},
		"device":{"name":"/dev/sdc","type":"sat","protocol":"ATA"},"power_mode":{"ata_value":0,"name":"STANDBY"}}`
	// A virtual disk in a VM has no SMART.
	qemuJSON = `{"smartctl":{"exit_status":4},"device":{"name":"/dev/sdd","type":"scsi","protocol":"SCSI"},
		"smart_support":{"available":false}}`
)

// fakeRoot makes /sys/block with the given devices; those in physical get
// a device link, like real hardware.
func fakeRoot(t *testing.T, physical []string, virtual []string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range physical {
		if err := os.MkdirAll(filepath.Join(root, "sys", "block", name, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range virtual {
		if err := os.MkdirAll(filepath.Join(root, "sys", "block", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakeCommands answers each command line from a table. A missing program
// fails like exec does.
type fakeCommands struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func (f *fakeCommands) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	for prefix, err := range f.errs {
		if strings.HasPrefix(line, prefix) {
			return []byte(f.outputs[prefix]), err
		}
	}
	for prefix, out := range f.outputs {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), nil
		}
	}
	return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func number(value float64) *float64 { return &value }

func smartctlCommand(device string) string {
	return "smartctl --json=c --nocheck=standby --info --health --attributes --log=devstat /dev/" + device
}

func TestReadSmart(t *testing.T) {
	root := fakeRoot(t,
		[]string{"nvme0n1", "nvme1n1", "sda", "sdb", "sdc", "sdd", "sr0"},
		[]string{"loop0", "dm-0", "zram0", "md0"})
	exitStatus := func(code int) error { return fmt.Errorf("exit status %d", code) }
	commands := &fakeCommands{
		outputs: map[string]string{
			smartctlCommand("nvme0n1"): nvmeJSON,
			smartctlCommand("nvme1n1"): nvmeOldJSON,
			smartctlCommand("sda"):     ataSSDJSON,
			smartctlCommand("sdb"):     ataHDDFailingJSON,
			smartctlCommand("sdc"):     ataStandbyJSON,
			smartctlCommand("sdd"):     qemuJSON,
		},
		errs: map[string]error{
			smartctlCommand("sdb"): exitStatus(8),
			smartctlCommand("sdc"): exitStatus(2),
			smartctlCommand("sdd"): exitStatus(4),
		},
	}
	disks, err := HostJobs{Root: root, Run: commands.run}.ReadSmart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ok, failing := "ok", "failing"
	want := []SmartDisk{
		{Device: "nvme0n1", Health: &ok, TemperatureCelsius: number(39), WearPercent: number(14)},
		{Device: "nvme1n1", Health: &ok, TemperatureCelsius: number(41.4), WearPercent: number(3)},
		{Device: "sda", Health: &ok, TemperatureCelsius: number(31), WearPercent: number(7)},
		// An HDD has no wear.
		{Device: "sdb", Health: &failing, TemperatureCelsius: number(44)},
		// Asleep: listed, but nothing read.
		{Device: "sdc", Standby: true},
		// sdd has no SMART; sr0 is not a disk; loop, dm, zram, md are virtual.
	}
	if got, wantText := fmt.Sprint(describe(disks)), fmt.Sprint(describe(want)); got != wantText {
		t.Errorf("disks =\n%s\nwant\n%s", got, wantText)
	}
	for _, call := range commands.calls {
		// Every read must skip a sleeping disk instead of waking it.
		if !strings.Contains(call, "--nocheck=standby") {
			t.Errorf("%q can wake a sleeping disk", call)
		}
		for _, name := range []string{"sr0", "loop0", "dm-0", "zram0", "md0"} {
			if strings.HasSuffix(call, "/dev/"+name) {
				t.Errorf("read %s", name)
			}
		}
	}
}

func describe(disks []SmartDisk) []string {
	var lines []string
	show := func(value any) string {
		switch v := value.(type) {
		case *string:
			if v != nil {
				return *v
			}
		case *float64:
			if v != nil {
				return fmt.Sprint(*v)
			}
		}
		return "null"
	}
	for _, disk := range disks {
		lines = append(lines, fmt.Sprintf("%s standby=%v health=%s temp=%s wear=%s\n", disk.Device, disk.Standby,
			show(disk.Health), show(disk.TemperatureCelsius), show(disk.WearPercent)))
	}
	return lines
}

func TestReadSmartSkipsVirtualDisks(t *testing.T) {
	// A hypervisor's emulated disks answer SMART with made-up values.
	root := fakeRoot(t, []string{"sda", "nvme0n1", "sdb"}, nil)
	commands := &fakeCommands{outputs: map[string]string{
		smartctlCommand("sda"):     strings.Replace(ataSSDJSON, `"rotation_rate":0,`, `"model_name":"QEMU HARDDISK","rotation_rate":0,`, 1),
		smartctlCommand("nvme0n1"): strings.Replace(nvmeJSON, `"smart_support"`, `"model_name":"QEMU NVMe Ctrl","smart_support"`, 1),
		smartctlCommand("sdb"):     strings.Replace(ataSSDJSON, `"rotation_rate":0,`, `"model_name":"VBOX HARDDISK","rotation_rate":0,`, 1),
	}}
	disks, err := HostJobs{Root: root, Run: commands.run}.ReadSmart(context.Background())
	if err != nil || len(disks) != 0 {
		t.Errorf("ReadSmart = %v, %v; want no disks", describe(disks), err)
	}
}

func TestReadSmartWithoutSmartctl(t *testing.T) {
	root := fakeRoot(t, []string{"sda"}, nil)
	disks, err := HostJobs{Root: root, Run: (&fakeCommands{}).run}.ReadSmart(context.Background())
	if err != nil || len(disks) != 0 {
		t.Errorf("ReadSmart = %v, %v; want no disks and no error", disks, err)
	}
}

func TestReadSmartSkipsNamesThatAreNotPlainDevices(t *testing.T) {
	root := fakeRoot(t, []string{"sda", "Bad-Name", "nvme0c0n1"}, nil)
	commands := &fakeCommands{outputs: map[string]string{"smartctl": ataSSDJSON}}
	HostJobs{Root: root, Run: commands.run}.ReadSmart(context.Background())
	if len(commands.calls) != 1 || !strings.HasSuffix(commands.calls[0], "/dev/sda") {
		t.Errorf("calls = %v, want only sda", commands.calls)
	}
}

func TestWearIsCappedAt100(t *testing.T) {
	root := fakeRoot(t, []string{"nvme0n1"}, nil)
	worn := strings.ReplaceAll(nvmeJSON, `"current_percent":14`, `"current_percent":180`)
	commands := &fakeCommands{outputs: map[string]string{"smartctl": worn}}
	disks, _ := HostJobs{Root: root, Run: commands.run}.ReadSmart(context.Background())
	if len(disks) != 1 || *disks[0].WearPercent != 100 {
		t.Errorf("disks = %v", describe(disks))
	}
}

const psFormat = "ps --all --no-trunc --format {{.ID}}\t{{.Names}}\t{{.State}}\t{{.Status}}"

func TestReadContainers(t *testing.T) {
	commands := &fakeCommands{outputs: map[string]string{
		"docker " + psFormat: "" +
			"aaa\tweb\trunning\tUp 3 hours (healthy)\n" +
			"bbb\tdb,app/db\trunning\tUp 3 hours (unhealthy)\n" +
			"ccc\told\texited\tExited (0) 2 days ago\n" +
			"ddd\tloop\trestarting\tRestarting (1) 5 seconds ago\n" +
			"eee\tnew\tcreated\tCreated\n",
		// The same container seen through a podman-docker shim is counted once.
		"podman " + psFormat: "" +
			"fff\tjellyfin\trunning\tUp 2 minutes\n" +
			"aaa\tweb\trunning\tUp 3 hours (healthy)\n" +
			"ggg\tpaused\tpaused\tPaused\n",
	}}
	containers, err := HostJobs{Run: commands.run}.ReadContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(containers.Engines, []string{"docker", "podman"}) {
		t.Errorf("engines = %v", containers.Engines)
	}
	want := []Container{
		{"db", StateUnhealthy},
		{"jellyfin", StateRunning},
		{"loop", StateUnhealthy},
		{"new", StateStopped},
		{"old", StateStopped},
		{"paused", StateStopped},
		{"web", StateRunning},
	}
	if !slices.Equal(containers.Items, want) {
		t.Errorf("containers =\n%v\nwant\n%v", containers.Items, want)
	}
}

func TestReadContainersWithoutEngines(t *testing.T) {
	containers, err := HostJobs{Run: (&fakeCommands{}).run}.ReadContainers(context.Background())
	if err != nil || len(containers.Engines) != 0 || containers.Items == nil || len(containers.Items) != 0 {
		t.Errorf("ReadContainers = %+v, %v; want no engines, an empty list, and no error", containers, err)
	}
}

func TestReadContainersWhenAnEngineFails(t *testing.T) {
	// The Docker daemon is stopped: Docker is still installed, but the
	// counts are unknown, since they would be wrong.
	commands := &fakeCommands{
		outputs: map[string]string{"docker ps": "", "podman " + psFormat: "fff\tjellyfin\trunning\tUp 2 minutes\n"},
		errs:    map[string]error{"docker ps": errors.New("exit status 1")},
	}
	containers, err := HostJobs{Run: commands.run}.ReadContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(containers.Engines, []string{"docker", "podman"}) || containers.Error == "" || len(containers.Items) != 0 {
		t.Errorf("ReadContainers = %+v; want both engines, an error, and no items", containers)
	}
}

// fakeStreams sends lines for each engine that is installed.
func fakeStreams(lines map[string][]string) func(ctx context.Context, line func(string), name string, args ...string) error {
	return func(ctx context.Context, line func(string), name string, args ...string) error {
		out, ok := lines[name]
		if !ok {
			return &exec.Error{Name: name, Err: exec.ErrNotFound}
		}
		for _, text := range out {
			line(text)
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestWatchContainersSignalsLifecycleEventsOnly(t *testing.T) {
	jobs := HostJobs{Stream: fakeStreams(map[string][]string{
		"docker": {"start", "exec_create: sh -c true", "exec_start: sh -c true", "exec_die", "health_status: unhealthy", "die"},
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var changes int
	jobs.WatchContainers(ctx, func() { changes++ })
	if changes != 3 {
		t.Errorf("changes = %d, want 3: health checks run commands in the container, which is no change", changes)
	}
}

func TestWatchContainersWithoutEnginesFails(t *testing.T) {
	jobs := HostJobs{Stream: fakeStreams(nil)}
	if err := jobs.WatchContainers(context.Background(), func() {}); err == nil {
		t.Error("no error without a container engine")
	}
}

func TestReadSMBIOSUUID(t *testing.T) {
	root := t.TempDir()
	jobs := HostJobs{Root: root}
	if uuid, err := jobs.ReadSMBIOSUUID(context.Background()); err != nil || uuid != nil {
		t.Errorf("without the file: %v, %v; want null", uuid, err)
	}
	dir := filepath.Join(root, "sys", "class", "dmi", "id")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "product_uuid"), []byte("4C4C4544-0042-3510-8053-B4C04F4E3632\n"), 0o600)
	if uuid, err := jobs.ReadSMBIOSUUID(context.Background()); err != nil || uuid == nil || *uuid != "4c4c4544-0042-3510-8053-b4c04f4e3632" {
		t.Errorf("ReadSMBIOSUUID = %v, %v", uuid, err)
	}
	os.WriteFile(filepath.Join(dir, "product_uuid"), []byte("Not Settable\n"), 0o600)
	if uuid, _ := jobs.ReadSMBIOSUUID(context.Background()); uuid != nil {
		t.Errorf("a value that is not a UUID gave %q, want null", *uuid)
	}
}
