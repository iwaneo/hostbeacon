package system

import (
	"path/filepath"
	"testing"
)

func addHwmon(t *testing.T, root, dir, name string, inputs map[string]string, labels map[string]string) {
	t.Helper()
	base := filepath.Join(root, "sys", "class", "hwmon", dir)
	writeFile(t, filepath.Join(base, "name"), name+"\n")
	for file, value := range inputs {
		writeFile(t, filepath.Join(base, file+"_input"), value+"\n")
	}
	for file, label := range labels {
		writeFile(t, filepath.Join(base, file+"_label"), label+"\n")
	}
}

func TestIntelPackageTemperature(t *testing.T) {
	root := t.TempDir()
	addHwmon(t, root, "hwmon0", "acpitz", map[string]string{"temp1": "27800"}, nil)
	addHwmon(t, root, "hwmon1", "nvme", map[string]string{"temp1": "38850"}, nil)
	addHwmon(t, root, "hwmon2", "coretemp",
		map[string]string{"temp1": "51600", "temp2": "70000", "temp3": "49000"},
		map[string]string{"temp1": "Package id 0", "temp2": "Core 0", "temp3": "Core 1"})
	sensor, ok := FindCPUTemperature(root)
	if !ok {
		t.Fatal("no CPU sensor found")
	}
	if got := sensor.Read(); got.CPUCelsius == nil || *got.CPUCelsius != 52 {
		t.Errorf("CPU temperature = %v, want 52 (the package, rounded)", got.CPUCelsius)
	}
}

func TestAMDPrefersTdie(t *testing.T) {
	root := t.TempDir()
	addHwmon(t, root, "hwmon3", "k10temp",
		map[string]string{"temp1": "65250", "temp2": "45250"},
		map[string]string{"temp1": "Tctl", "temp2": "Tdie"})
	sensor, _ := FindCPUTemperature(root)
	if got := sensor.Read(); got.CPUCelsius == nil || *got.CPUCelsius != 45 {
		t.Errorf("CPU temperature = %v, want 45 (Tdie)", got.CPUCelsius)
	}
}

func TestRaspberryPiThermalZone(t *testing.T) {
	root := t.TempDir()
	zone := filepath.Join(root, "sys", "class", "thermal", "thermal_zone0")
	writeFile(t, filepath.Join(zone, "type"), "cpu-thermal\n")
	writeFile(t, filepath.Join(zone, "temp"), "48312\n")
	sensor, ok := FindCPUTemperature(root)
	if !ok {
		t.Fatal("no CPU sensor found")
	}
	if got := sensor.Read(); got.CPUCelsius == nil || *got.CPUCelsius != 48 {
		t.Errorf("CPU temperature = %v, want 48", got.CPUCelsius)
	}
}

func TestNoCPUSensor(t *testing.T) {
	// A VM often has only the ACPI zone, which is not the CPU.
	root := t.TempDir()
	addHwmon(t, root, "hwmon0", "acpitz", map[string]string{"temp1": "27800"}, nil)
	if _, ok := FindCPUTemperature(root); ok {
		t.Error("found a CPU sensor, want none")
	}
}

func TestUnreadableSensorIsNull(t *testing.T) {
	root := t.TempDir()
	addHwmon(t, root, "hwmon0", "coretemp", map[string]string{"temp1": "51600"}, nil)
	sensor, _ := FindCPUTemperature(root)
	writeFile(t, filepath.Join(root, "sys", "class", "hwmon", "hwmon0", "temp1_input"), "error\n")
	if got := sensor.Read(); got.CPUCelsius != nil {
		t.Errorf("CPU temperature = %v, want null", *got.CPUCelsius)
	}
}
