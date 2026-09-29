package system

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeProc(t *testing.T, root, name, content string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "proc", name), content)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const meminfo = `MemTotal:        8000000 kB
MemFree:          500000 kB
MemAvailable:    2000000 kB
Buffers:          100000 kB
SwapTotal:       1000000 kB
SwapFree:         666000 kB
`

func TestSample(t *testing.T) {
	root := t.TempDir()
	// user nice system idle iowait irq softirq steal guest guest_nice
	writeProc(t, root, "stat", "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3\n")
	writeProc(t, root, "meminfo", meminfo)
	writeProc(t, root, "loadavg", "0.42 1.234 12.50 1/123 4567\n")
	s := NewSampler(root, true)

	first := s.Sample()
	if first.CPUPercent != nil {
		t.Errorf("first CPU = %v, want null (no earlier reading)", *first.CPUPercent)
	}
	if first.MemoryPercent == nil || *first.MemoryPercent != 75 {
		t.Errorf("memory = %v, want 75", first.MemoryPercent)
	}
	if first.MemoryUsedBytes == nil || *first.MemoryUsedBytes != 6_140_000_000 {
		t.Errorf("memory used = %v, want 6140000000 (6000000 kB to 3 digits)", first.MemoryUsedBytes)
	}
	if first.SwapPercent == nil || *first.SwapPercent != 33 {
		t.Errorf("swap = %v, want 33 (33.4 rounded)", first.SwapPercent)
	}
	if first.Load1 == nil || *first.Load1 != 0.42 || *first.Load5 != 1.23 || *first.Load15 != 12.5 {
		t.Errorf("load = %v %v %v, want 0.42 1.23 12.5 (2 decimals)", first.Load1, first.Load5, first.Load15)
	}

	// 300 more busy, 600 more idle (idle + iowait): 33.3 % busy.
	writeProc(t, root, "stat", "cpu  300 0 200 1200 200 0 0 0 50 0\n")
	second := s.Sample()
	if second.CPUPercent == nil || *second.CPUPercent != 33 {
		t.Errorf("CPU = %v, want 33", second.CPUPercent)
	}
}

func TestNoSwapIsNull(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "stat", "cpu  1 0 1 1 0 0 0 0 0 0\n")
	writeProc(t, root, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 500 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	if swap := NewSampler(root, true).Sample().SwapPercent; swap != nil {
		t.Errorf("swap = %v, want null on a Host without swap", *swap)
	}
}

func TestUnreadableProcGivesNulls(t *testing.T) {
	sample := NewSampler(t.TempDir(), true).Sample()
	if sample.CPUPercent != nil || sample.MemoryPercent != nil || sample.MemoryUsedBytes != nil || sample.SwapPercent != nil || sample.Load1 != nil {
		t.Errorf("sample = %+v, want all null", sample)
	}
}

func TestLoadIsHiddenWhenNotReported(t *testing.T) {
	// In LXC, load shows the hypervisor's values, so the Agent does not send it.
	root := t.TempDir()
	writeProc(t, root, "loadavg", "0.42 1.23 12.50 1/123 4567\n")
	if sample := NewSampler(root, false).Sample(); sample.Load1 != nil || sample.Load5 != nil || sample.Load15 != nil {
		t.Errorf("load = %v %v %v, want null", sample.Load1, sample.Load5, sample.Load15)
	}
}

func TestSameValuesGiveEqualSamples(t *testing.T) {
	// Rounding hides small changes, so no delta is sent for them.
	root := t.TempDir()
	writeProc(t, root, "stat", "cpu  100 0 100 700 100 0 0 0 0 0\n")
	writeProc(t, root, "meminfo", meminfo)
	writeProc(t, root, "loadavg", "0.421 0 0 1/1 1\n")
	s := NewSampler(root, true)
	s.Sample()
	writeProc(t, root, "stat", "cpu  200 0 200 1500 200 0 0 0 0 0\n")
	first := s.Sample()
	writeProc(t, root, "stat", "cpu  300 0 300 2301 300 0 0 0 0 0\n")
	writeProc(t, root, "meminfo", strings.Replace(meminfo, "2000000", "2000100", 1))
	writeProc(t, root, "loadavg", "0.419 0 0 1/1 1\n")
	if second := s.Sample(); !reflect.DeepEqual(first, second) {
		t.Errorf("samples differ after rounding: %+v and %+v", first, second)
	}
}
