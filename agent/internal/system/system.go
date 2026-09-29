// Package system reads the system group: CPU, memory, and swap. It needs no
// root. Values are rounded, so a delta is sent only when a shown value
// changes: percentages to whole numbers, byte counts to 3 significant digits.
package system

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// Sampler reads /proc under a root directory ("/" on a Host).
type Sampler struct {
	root    string
	lastCPU *cpuTimes
}

type cpuTimes struct{ busy, total uint64 }

// NewSampler reads /proc under root.
func NewSampler(root string) *Sampler {
	return &Sampler{root: root}
}

// Sample reads the system group. CPU usage is the average since the last
// Sample, so the first one has no CPU value. A value that cannot be read is
// null. Load is not read yet.
func (s *Sampler) Sample() protocol.System {
	var sample protocol.System
	if cpu, ok := s.readCPU(); ok {
		if s.lastCPU != nil && cpu.total > s.lastCPU.total {
			busy := float64(cpu.busy-s.lastCPU.busy) / float64(cpu.total-s.lastCPU.total)
			sample.CPUPercent = percent(busy)
		}
		s.lastCPU = &cpu
	}
	memory := s.readMeminfo()
	if total, available := memory["MemTotal"], memory["MemAvailable"]; total > 0 && available <= total {
		used := total - available
		sample.MemoryPercent = percent(float64(used) / float64(total))
		bytes := int64(math.Round(significant(float64(used)*1024, 3)))
		sample.MemoryUsedBytes = &bytes
	}
	if total, free := memory["SwapTotal"], memory["SwapFree"]; total > 0 && free <= total {
		sample.SwapPercent = percent(float64(total-free) / float64(total))
	}
	return sample
}

// readCPU reads the total line of /proc/stat. Idle time is idle plus iowait;
// guest time is already inside user and nice.
func (s *Sampler) readCPU() (cpuTimes, bool) {
	data, err := os.ReadFile(filepath.Join(s.root, "proc", "stat"))
	if err != nil {
		return cpuTimes{}, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 9 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	var times cpuTimes
	for i, field := range fields[1:9] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		times.total += value
		if i != 3 && i != 4 { // idle, iowait
			times.busy += value
		}
	}
	return times, true
}

// readMeminfo returns /proc/meminfo in kB.
func (s *Sampler) readMeminfo() map[string]uint64 {
	values := map[string]uint64{}
	file, err := os.Open(filepath.Join(s.root, "proc", "meminfo"))
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		if value, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			values[name] = value
		}
	}
	return values
}

func percent(fraction float64) *float64 {
	value := math.Round(fraction * 100)
	return &value
}

// significant rounds value to digits significant digits.
func significant(value float64, digits int) float64 {
	if value == 0 {
		return 0
	}
	scale := math.Pow(10, float64(digits)-math.Ceil(math.Log10(math.Abs(value))))
	return math.Round(value*scale) / scale
}
