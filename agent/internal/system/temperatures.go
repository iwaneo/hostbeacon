package system

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// cpuHwmon are the hwmon drivers that measure the CPU.
var cpuHwmon = []string{"coretemp", "k10temp", "zenpower", "cpu_thermal", "cpu-thermal", "soc_thermal"}

// cpuThermalZones are the thermal zone types of the CPU.
var cpuThermalZones = []string{"x86_pkg_temp", "cpu-thermal", "cpu_thermal", "soc-thermal", "soc_thermal", "cpu0-thermal"}

// CPUTemperature is the files that hold the CPU temperature, in millidegrees.
// When there are several (one per CPU package), the highest counts.
type CPUTemperature struct {
	files []string
}

// FindCPUTemperature looks for the CPU sensor in hwmon, then in the thermal
// zones. A Host without one (most VMs) has no temperatures capability.
func FindCPUTemperature(root string) (CPUTemperature, bool) {
	dirs, _ := filepath.Glob(filepath.Join(root, "sys", "class", "hwmon", "hwmon*"))
	for _, dir := range dirs {
		if name := readTrimmed(filepath.Join(dir, "name")); slices.Contains(cpuHwmon, name) {
			if files := hwmonCPUFiles(dir); len(files) > 0 {
				return CPUTemperature{files: files}, true
			}
		}
	}
	zones, _ := filepath.Glob(filepath.Join(root, "sys", "class", "thermal", "thermal_zone*"))
	var files []string
	for _, zone := range zones {
		if slices.Contains(cpuThermalZones, readTrimmed(filepath.Join(zone, "type"))) {
			files = append(files, filepath.Join(zone, "temp"))
		}
	}
	return CPUTemperature{files: files}, len(files) > 0
}

// hwmonCPUFiles picks the inputs that stand for the whole CPU: Intel
// "Package id N", AMD "Tdie" (else "Tctl"), else every input.
func hwmonCPUFiles(dir string) []string {
	inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
	byLabel := map[string][]string{}
	for _, input := range inputs {
		label := readTrimmed(strings.TrimSuffix(input, "_input") + "_label")
		if strings.HasPrefix(label, "Package id") {
			label = "Package"
		}
		byLabel[label] = append(byLabel[label], input)
	}
	for _, label := range []string{"Package", "Tdie", "Tctl"} {
		if files := byLabel[label]; len(files) > 0 {
			return files
		}
	}
	return inputs
}

// Read reads the temperatures group. It is null when no file can be read.
func (c CPUTemperature) Read() protocol.Temperatures {
	var highest *float64
	for _, file := range c.files {
		value, err := strconv.ParseFloat(readTrimmed(file), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		celsius := math.Round(value / 1000)
		if highest == nil || celsius > *highest {
			highest = &celsius
		}
	}
	return protocol.Temperatures{CPUCelsius: highest}
}

func readTrimmed(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}
