package hwmon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestK10tempCollector(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "hwmon9")
	must(t, os.MkdirAll(hwmon, 0o755))
	writeFile(t, hwmon, "name", "k10temp")
	writeFile(t, hwmon, "temp1_input", "51125")
	writeFile(t, hwmon, "temp1_label", "Tctl")
	writeFile(t, hwmon, "temp3_input", "49000")
	writeFile(t, hwmon, "temp3_label", "Tccd1")

	c := NewCollector(root)
	mfs := collect(t, c)

	cpuMF, ok := mfs["thermalscope_cpu_temperature_celsius"]
	if !ok {
		t.Fatal("metric thermalscope_cpu_temperature_celsius not found")
	}
	if got := len(cpuMF.GetMetric()); got != 2 {
		t.Fatalf("expected 2 CPU temp metrics, got %d", got)
	}
	for _, m := range cpuMF.GetMetric() {
		label := labelVal(m, "sensor")
		switch label {
		case "Tctl":
			if got := m.GetGauge().GetValue(); got != 51.125 {
				t.Errorf("Tctl: got %v, want 51.125", got)
			}
		case "Tccd1":
			if got := m.GetGauge().GetValue(); got != 49.0 {
				t.Errorf("Tccd1: got %v, want 49.0", got)
			}
		default:
			t.Errorf("unexpected sensor label %q", label)
		}
	}
}

func TestCollectorUp(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "hwmon9")
	must(t, os.MkdirAll(hwmon, 0o755))
	writeFile(t, hwmon, "name", "k10temp")
	writeFile(t, hwmon, "temp1_input", "50000")
	writeFile(t, hwmon, "temp1_label", "Tctl")

	c := NewCollector(root)
	mfs := collect(t, c)

	upMF, ok := mfs["thermalscope_hwmon_up"]
	if !ok {
		t.Fatal("thermalscope_hwmon_up not found")
	}
	if got := upMF.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Errorf("hwmon_up: got %v, want 1", got)
	}
}

func TestNVMeCollectorModernLayout(t *testing.T) {
	// Modern kernels (6.x): /sys/class/hwmon/hwmonN/device -> /sys/devices/.../nvme/nvmeN
	// i.e. the controller itself, not the PCI parent.
	root, hwmonRoot := setupSysTree(t)

	// /sys/devices/pci0000:c0/0000:c0:01.1/0000:c1:00.0/nvme/nvme0
	pci := filepath.Join(root, "devices", "pci0000:c0", "0000:c0:01.1", "0000:c1:00.0")
	nvmeCtrl := filepath.Join(pci, "nvme", "nvme0")
	must(t, os.MkdirAll(nvmeCtrl, 0o755))

	// hwmon0 lives under the controller and its `device` symlink points at the controller.
	hwmon := filepath.Join(nvmeCtrl, "hwmon0")
	must(t, os.MkdirAll(hwmon, 0o755))
	writeFile(t, hwmon, "name", "nvme")
	writeFile(t, hwmon, "temp1_input", "42000")
	writeFile(t, hwmon, "temp2_input", "44500")
	must(t, os.Symlink(nvmeCtrl, filepath.Join(hwmon, "device")))

	// /sys/class/hwmon/hwmon0 -> the controller-owned hwmon dir
	must(t, os.Symlink(hwmon, filepath.Join(hwmonRoot, "hwmon0")))
	// /sys/class/nvme/nvme0 -> the controller
	must(t, os.Symlink(nvmeCtrl, filepath.Join(root, "class", "nvme", "nvme0")))

	mfs := collect(t, NewCollector(hwmonRoot))
	assertNVMeDevice(t, mfs, "nvme0", 42.0, 44.5)
}

func TestNVMeCollectorLegacyLayout(t *testing.T) {
	// Legacy: hwmonN/device -> PCI parent; nvmeN lives at <pci>/nvme/nvmeN.
	root, hwmonRoot := setupSysTree(t)

	pci := filepath.Join(root, "devices", "pci0000:00", "0000:00:01.1", "0000:01:00.0")
	nvmeCtrl := filepath.Join(pci, "nvme", "nvme5")
	must(t, os.MkdirAll(nvmeCtrl, 0o755))

	hwmon := filepath.Join(pci, "hwmon", "hwmon7")
	must(t, os.MkdirAll(hwmon, 0o755))
	writeFile(t, hwmon, "name", "nvme")
	writeFile(t, hwmon, "temp1_input", "38000")
	must(t, os.Symlink(pci, filepath.Join(hwmon, "device")))

	must(t, os.Symlink(hwmon, filepath.Join(hwmonRoot, "hwmon7")))

	mfs := collect(t, NewCollector(hwmonRoot))
	assertNVMeDevice(t, mfs, "nvme5", 38.0, 0)
}

func TestNVMeCollectorInverseFallback(t *testing.T) {
	// device symlink is missing or weird; the inverse walk via /sys/class/nvme
	// finds a sibling hwmon whose basename matches.
	root, hwmonRoot := setupSysTree(t)

	pci := filepath.Join(root, "devices", "pci0000:c0", "0000:c0:03.1", "0000:c5:00.0")
	nvmeCtrl := filepath.Join(pci, "nvme", "nvme4")
	must(t, os.MkdirAll(nvmeCtrl, 0o755))

	// hwmon dir owned by the controller, but no `device` symlink at all.
	hwmonCtrl := filepath.Join(nvmeCtrl, "hwmon4")
	must(t, os.MkdirAll(hwmonCtrl, 0o755))
	writeFile(t, hwmonCtrl, "name", "nvme")
	writeFile(t, hwmonCtrl, "temp1_input", "55000")

	must(t, os.Symlink(hwmonCtrl, filepath.Join(hwmonRoot, "hwmon4")))
	// /sys/class/nvme/nvme4 -> controller, which contains hwmon4
	must(t, os.Symlink(nvmeCtrl, filepath.Join(root, "class", "nvme", "nvme4")))

	mfs := collect(t, NewCollector(hwmonRoot))
	assertNVMeDevice(t, mfs, "nvme4", 55.0, 0)
}

func TestNVMeCollectorFallbackToHwmonBase(t *testing.T) {
	// Nothing resolvable — the collector should still emit metrics labelled
	// with the hwmon basename so we don't drop data on exotic kernels.
	_, hwmonRoot := setupSysTree(t)
	hwmon := filepath.Join(hwmonRoot, "hwmon2")
	must(t, os.MkdirAll(hwmon, 0o755))
	writeFile(t, hwmon, "name", "nvme")
	writeFile(t, hwmon, "temp1_input", "40000")

	mfs := collect(t, NewCollector(hwmonRoot))
	assertNVMeDevice(t, mfs, "hwmon2", 40.0, 0)
}

// setupSysTree builds a fake /sys layout under t.TempDir():
//
//	<root>/class/hwmon/   <- returned as hwmonRoot
//	<root>/class/nvme/
//	<root>/devices/...    <- callers populate as needed
func setupSysTree(t *testing.T) (root, hwmonRoot string) {
	t.Helper()
	root = t.TempDir()
	hwmonRoot = filepath.Join(root, "class", "hwmon")
	must(t, os.MkdirAll(hwmonRoot, 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "class", "nvme"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "devices"), 0o755))
	return root, hwmonRoot
}

func assertNVMeDevice(t *testing.T, mfs map[string]*dto.MetricFamily, wantDevice string, wantTemp1, wantTemp2 float64) {
	t.Helper()
	mf, ok := mfs["thermalscope_nvme_temperature_celsius"]
	if !ok {
		t.Fatal("metric thermalscope_nvme_temperature_celsius not found")
	}
	var sawTemp1, sawTemp2 bool
	for _, m := range mf.GetMetric() {
		dev := labelVal(m, "device")
		if dev != wantDevice {
			t.Errorf("device label: got %q, want %q", dev, wantDevice)
			continue
		}
		switch labelVal(m, "sensor") {
		case "Composite":
			sawTemp1 = true
			if got := m.GetGauge().GetValue(); got != wantTemp1 {
				t.Errorf("Composite: got %v, want %v", got, wantTemp1)
			}
		case "Sensor1":
			sawTemp2 = true
			if got := m.GetGauge().GetValue(); got != wantTemp2 {
				t.Errorf("Sensor1: got %v, want %v", got, wantTemp2)
			}
		}
	}
	if wantTemp1 != 0 && !sawTemp1 {
		t.Errorf("missing Composite metric for %s", wantDevice)
	}
	if wantTemp2 != 0 && !sawTemp2 {
		t.Errorf("missing Sensor1 metric for %s", wantDevice)
	}
}

func TestUnreadableHwmonRoot(t *testing.T) {
	c := NewCollector("/nonexistent/hwmon/path")
	mfs := collect(t, c)
	upMF, ok := mfs["thermalscope_hwmon_up"]
	if !ok {
		t.Fatal("thermalscope_hwmon_up not found when root missing")
	}
	if got := upMF.GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Errorf("expected hwmon_up=0 for missing root, got %v", got)
	}
}

func collect(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

func labelVal(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
