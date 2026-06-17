package power

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// raplDomain describes a single intel-rapl:* directory to lay down in a fake
// /sys/class/powercap tree.
type raplDomain struct {
	dir      string // e.g. "intel-rapl:0" or "intel-rapl:0:0"
	name     string // contents of the `name` file, e.g. "package-0", "core"
	energy   string // contents of `energy_uj`
	maxRange string // contents of `max_energy_range_uj`; "" omits the file
}

func TestRAPLCollectorMultiDomain(t *testing.T) {
	// Mirrors the verified layout on talos-18u-ski:
	//   intel-rapl:0   -> package-0
	//   intel-rapl:0:0 -> core
	root := setupPowercap(t, []raplDomain{
		{dir: "intel-rapl:0", name: "package-0", energy: "31986573028", maxRange: "262143328850"},
		{dir: "intel-rapl:0:0", name: "core", energy: "27782060162", maxRange: "262143328850"},
		// A non-RAPL sibling that must be ignored.
		{dir: "intel-rapl-mmio:0", name: "package-0-mmio", energy: "999", maxRange: "262143328850"},
	})

	mfs := collect(t, NewCollector(root))

	mf, ok := mfs["thermalscope_rapl_energy_microjoules_total"]
	if !ok {
		t.Fatal("metric thermalscope_rapl_energy_microjoules_total not found")
	}
	if got := len(mf.GetMetric()); got != 2 {
		t.Fatalf("expected 2 RAPL domains (mmio ignored), got %d", got)
	}
	seen := map[string]float64{}
	for _, m := range mf.GetMetric() {
		seen[labelVal(m, "domain")] = m.GetCounter().GetValue()
	}
	if got, want := seen["package-0"], 31986573028.0; got != want {
		t.Errorf("package-0: got %v, want %v", got, want)
	}
	if got, want := seen["core"], 27782060162.0; got != want {
		t.Errorf("core: got %v, want %v", got, want)
	}

	assertUp(t, mfs, 1)
}

func TestRAPLCounterIsCounterType(t *testing.T) {
	root := setupPowercap(t, []raplDomain{
		{dir: "intel-rapl:0", name: "package-0", energy: "100", maxRange: "1000"},
	})
	mfs := collect(t, NewCollector(root))
	mf := mfs["thermalscope_rapl_energy_microjoules_total"]
	if mf.GetType() != dto.MetricType_COUNTER {
		t.Errorf("expected COUNTER type, got %v", mf.GetType())
	}
}

func TestRAPLWraparound(t *testing.T) {
	// energy_uj wraps at max_energy_range_uj. Across three scrapes the raw
	// value goes up, then drops (wrap), then climbs again. The exported value
	// must be strictly monotonic with the ceiling added on each wrap.
	root := setupPowercap(t, []raplDomain{
		{dir: "intel-rapl:0", name: "package-0", energy: "900", maxRange: "1000"},
	})
	c := NewCollector(root)
	dir := filepath.Join(root, "intel-rapl:0")

	// Scrape 1: raw=900 -> exported 900.
	if got := scrapeEnergy(t, c, "package-0"); got != 900 {
		t.Fatalf("scrape 1: got %v, want 900", got)
	}

	// Scrape 2: raw=950 (no wrap) -> exported 950.
	writeFile(t, dir, "energy_uj", "950")
	if got := scrapeEnergy(t, c, "package-0"); got != 950 {
		t.Fatalf("scrape 2: got %v, want 950", got)
	}

	// Scrape 3: raw=120 (< previous 950 => wrapped) -> exported 1000+120=1120.
	writeFile(t, dir, "energy_uj", "120")
	if got := scrapeEnergy(t, c, "package-0"); got != 1120 {
		t.Fatalf("scrape 3 (wrap): got %v, want 1120", got)
	}

	// Scrape 4: raw=300 (no further wrap) -> exported 1000+300=1300.
	writeFile(t, dir, "energy_uj", "300")
	if got := scrapeEnergy(t, c, "package-0"); got != 1300 {
		t.Fatalf("scrape 4: got %v, want 1300", got)
	}
}

func TestRAPLUnreadableRoot(t *testing.T) {
	c := NewCollector("/nonexistent/powercap/path")
	mfs := collect(t, c)
	assertUp(t, mfs, 0)
	if _, ok := mfs["thermalscope_rapl_energy_microjoules_total"]; ok {
		t.Error("expected no energy metrics when powercap root is unreadable")
	}
}

// scrapeEnergy gathers once and returns the counter value for a single domain.
func scrapeEnergy(t *testing.T, c prometheus.Collector, domain string) float64 {
	t.Helper()
	mfs := collect(t, c)
	mf, ok := mfs["thermalscope_rapl_energy_microjoules_total"]
	if !ok {
		t.Fatalf("energy metric missing for domain %q", domain)
	}
	for _, m := range mf.GetMetric() {
		if labelVal(m, "domain") == domain {
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("domain %q not found in energy metric", domain)
	return 0
}

// setupPowercap builds a fake /sys/class/powercap tree under t.TempDir().
func setupPowercap(t *testing.T, domains []raplDomain) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range domains {
		dir := filepath.Join(root, d.dir)
		must(t, os.MkdirAll(dir, 0o755))
		writeFile(t, dir, "name", d.name)
		writeFile(t, dir, "energy_uj", d.energy)
		if d.maxRange != "" {
			writeFile(t, dir, "max_energy_range_uj", d.maxRange)
		}
	}
	return root
}

func assertUp(t *testing.T, mfs map[string]*dto.MetricFamily, want float64) {
	t.Helper()
	upMF, ok := mfs["thermalscope_power_up"]
	if !ok {
		t.Fatal("thermalscope_power_up not found")
	}
	if got := upMF.GetMetric()[0].GetGauge().GetValue(); got != want {
		t.Errorf("power_up: got %v, want %v", got, want)
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
