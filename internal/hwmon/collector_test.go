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
