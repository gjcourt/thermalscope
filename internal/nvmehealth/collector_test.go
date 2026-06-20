package nvmehealth

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// makeSMARTLog builds a 512-byte SMART/Health log page with the given fields.
func makeSMARTLog(criticalWarning, availSpare, availSpareThresh, pctUsed uint8,
	dataWritten, powerOnHours, mediaErrors uint64) []byte {
	b := make([]byte, smartLogLen)
	b[0] = criticalWarning
	b[3] = availSpare
	b[4] = availSpareThresh
	b[5] = pctUsed
	binary.LittleEndian.PutUint64(b[48:56], dataWritten) // data_units_written low word
	binary.LittleEndian.PutUint64(b[128:136], powerOnHours)
	binary.LittleEndian.PutUint64(b[160:168], mediaErrors)
	return b
}

// gather collects all emitted metrics into a name->value map keyed by the
// device label where present (e.g. "thermalscope_nvme_percentage_used_ratio/nvme0").
func gather(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			key := mf.GetName()
			for _, l := range m.GetLabel() {
				if l.GetName() == "device" {
					key += "/" + l.GetValue()
				}
			}
			out[key] = metricValue(m)
		}
	}
	return out
}

func metricValue(m *dto.Metric) float64 {
	switch {
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	default:
		return 0
	}
}

func newTestCollector(t *testing.T, controllers []string, read func(string) ([]byte, error)) *Collector {
	t.Helper()
	sysClass := t.TempDir()
	for _, name := range controllers {
		if err := os.Mkdir(filepath.Join(sysClass, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	return &Collector{sysClassNVMe: sysClass, devDir: "/dev", read: read}
}

func TestCollect_ParsesAndEmits(t *testing.T) {
	log := makeSMARTLog(0, 100, 10, 7, 123456, 4200, 0)
	c := newTestCollector(t, []string{"nvme0"}, func(string) ([]byte, error) { return log, nil })

	got := gather(t, c)

	checks := map[string]float64{
		"thermalscope_nvme_percentage_used_ratio/nvme0":           0.07,
		"thermalscope_nvme_available_spare_ratio/nvme0":           1.0,
		"thermalscope_nvme_available_spare_threshold_ratio/nvme0": 0.10,
		"thermalscope_nvme_critical_warning/nvme0":                0,
		"thermalscope_nvme_data_units_written_total/nvme0":        123456,
		"thermalscope_nvme_power_on_hours_total/nvme0":            4200,
		"thermalscope_nvme_media_errors_total/nvme0":              0,
		"thermalscope_nvmehealth_up":                              1,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestCollect_WornDriveWithWarning(t *testing.T) {
	// 95% used, spare down to 8% (below the 10% threshold), critical_warning
	// bit0 (spare) set, media errors present.
	log := makeSMARTLog(0x01, 8, 10, 95, 0, 0, 42)
	c := newTestCollector(t, []string{"nvme0"}, func(string) ([]byte, error) { return log, nil })

	got := gather(t, c)
	if got["thermalscope_nvme_percentage_used_ratio/nvme0"] != 0.95 {
		t.Errorf("percentage_used = %v, want 0.95", got["thermalscope_nvme_percentage_used_ratio/nvme0"])
	}
	if got["thermalscope_nvme_critical_warning/nvme0"] != 1 {
		t.Errorf("critical_warning = %v, want 1", got["thermalscope_nvme_critical_warning/nvme0"])
	}
	if got["thermalscope_nvme_media_errors_total/nvme0"] != 42 {
		t.Errorf("media_errors = %v, want 42", got["thermalscope_nvme_media_errors_total/nvme0"])
	}
}

func TestCollect_SkipsNamespacesAndFailedReads(t *testing.T) {
	// nvme0n1 is a namespace, not a controller — must be ignored. nvme1 is a
	// controller whose read fails — it must be skipped without failing the
	// whole collector (up still 1).
	calls := map[string]int{}
	read := func(dev string) ([]byte, error) {
		calls[dev]++
		if filepath.Base(dev) == "nvme1" {
			return nil, errors.New("device busy")
		}
		return makeSMARTLog(0, 100, 10, 1, 0, 0, 0), nil
	}
	c := newTestCollector(t, []string{"nvme0", "nvme0n1", "nvme1"}, read)

	got := gather(t, c)

	if _, opened := calls[filepath.Join("/dev", "nvme0n1")]; opened {
		t.Error("namespace nvme0n1 should not be opened as a controller")
	}
	if got["thermalscope_nvmehealth_up"] != 1 {
		t.Error("up should be 1 even when one device read fails")
	}
	if _, ok := got["thermalscope_nvme_percentage_used_ratio/nvme0"]; !ok {
		t.Error("nvme0 metrics should be present")
	}
	if _, ok := got["thermalscope_nvme_percentage_used_ratio/nvme1"]; ok {
		t.Error("nvme1 metrics should be absent (read failed)")
	}
}

func TestCollect_ReadErrorCounterAccumulates(t *testing.T) {
	// nvme0 reads cleanly; nvme1's read always fails. The per-device read-error
	// counter must increment for nvme1 on each scrape, report a 0 baseline for
	// the healthy nvme0, and the collector must stay up.
	read := func(dev string) ([]byte, error) {
		if filepath.Base(dev) == "nvme1" {
			return nil, errors.New("operation not permitted")
		}
		return makeSMARTLog(0, 100, 10, 1, 0, 0, 0), nil
	}
	c := newTestCollector(t, []string{"nvme0", "nvme1"}, read)

	first := gather(t, c)
	if first["thermalscope_nvme_read_errors_total/nvme1"] < 1 {
		t.Errorf("nvme1 read_errors_total = %v, want >=1 after a failed read",
			first["thermalscope_nvme_read_errors_total/nvme1"])
	}
	if v, ok := first["thermalscope_nvme_read_errors_total/nvme0"]; !ok || v != 0 {
		t.Errorf("healthy nvme0 read_errors_total = %v (present=%v), want 0", v, ok)
	}
	if first["thermalscope_nvmehealth_up"] != 1 {
		t.Errorf("up = %v, want 1 even when a device read fails", first["thermalscope_nvmehealth_up"])
	}

	second := gather(t, c)
	// Exactly +1 per scrape — proves the counter is not double-counted within
	// a single Collect (e.g. by emitting per-device and per-map).
	if got, want := second["thermalscope_nvme_read_errors_total/nvme1"],
		first["thermalscope_nvme_read_errors_total/nvme1"]+1; got != want {
		t.Errorf("nvme1 read_errors_total = %v on second scrape, want exactly first+1 (%v)", got, want)
	}
	if second["thermalscope_nvme_read_errors_total/nvme0"] != 0 {
		t.Errorf("healthy nvme0 read_errors_total = %v on second scrape, want 0",
			second["thermalscope_nvme_read_errors_total/nvme0"])
	}
}

func TestCollect_DeviceDisappearsPrunesSeries(t *testing.T) {
	// Scrape 1: nvme0 healthy, nvme1 present and failing. Scrape 2: nvme1 has
	// disappeared from /sys/class/nvme. Its read_errors_total series must no
	// longer be emitted and its map entry must be pruned (no frozen series, no
	// unbounded growth).
	sysClass := t.TempDir()
	for _, name := range []string{"nvme0", "nvme1"} {
		if err := os.Mkdir(filepath.Join(sysClass, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	read := func(dev string) ([]byte, error) {
		if filepath.Base(dev) == "nvme1" {
			return nil, errors.New("operation not permitted")
		}
		return makeSMARTLog(0, 100, 10, 1, 0, 0, 0), nil
	}
	c := &Collector{sysClassNVMe: sysClass, devDir: "/dev", read: read}

	first := gather(t, c)
	if first["thermalscope_nvme_read_errors_total/nvme1"] != 1 {
		t.Fatalf("nvme1 read_errors_total = %v on scrape 1, want 1",
			first["thermalscope_nvme_read_errors_total/nvme1"])
	}

	// nvme1 disappears.
	if err := os.Remove(filepath.Join(sysClass, "nvme1")); err != nil {
		t.Fatalf("remove nvme1: %v", err)
	}

	second := gather(t, c)
	if _, ok := second["thermalscope_nvme_read_errors_total/nvme1"]; ok {
		t.Errorf("nvme1 read_errors_total still emitted after device disappeared: %v",
			second["thermalscope_nvme_read_errors_total/nvme1"])
	}
	if second["thermalscope_nvme_read_errors_total/nvme0"] != 0 {
		t.Errorf("nvme0 read_errors_total = %v after nvme1 disappeared, want 0",
			second["thermalscope_nvme_read_errors_total/nvme0"])
	}

	// The internal map must not retain the stale device.
	c.mu.Lock()
	if _, ok := c.readErrors["nvme1"]; ok {
		c.mu.Unlock()
		t.Error("readErrors map still retains nvme1 after it disappeared")
	} else {
		c.mu.Unlock()
	}
}

func TestCollect_ShortLogPageCountsAsReadError(t *testing.T) {
	// A short/partial log page is treated as a failed read: it must increment
	// read_errors_total and not emit health metrics.
	read := func(string) ([]byte, error) { return make([]byte, smartLogLen-1), nil }
	c := newTestCollector(t, []string{"nvme0"}, read)

	got := gather(t, c)
	if got["thermalscope_nvme_read_errors_total/nvme0"] != 1 {
		t.Errorf("short-page read_errors_total = %v, want 1",
			got["thermalscope_nvme_read_errors_total/nvme0"])
	}
	if _, ok := got["thermalscope_nvme_percentage_used_ratio/nvme0"]; ok {
		t.Error("health metrics should be absent for a short log page")
	}
	if got["thermalscope_nvmehealth_up"] != 1 {
		t.Errorf("up = %v, want 1 on a short-page read", got["thermalscope_nvmehealth_up"])
	}
}

func TestCollect_ConcurrentCollect(t *testing.T) {
	// Regression guard for the concurrency risk: many goroutines calling
	// Collect at once must not race on the shared maps. Run under -race.
	read := func(dev string) ([]byte, error) {
		if filepath.Base(dev) == "nvme1" {
			return nil, errors.New("operation not permitted")
		}
		return makeSMARTLog(0, 100, 10, 1, 0, 0, 0), nil
	}
	c := newTestCollector(t, []string{"nvme0", "nvme1"}, read)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				ch := make(chan prometheus.Metric, 64)
				go func() {
					for range ch {
					}
				}()
				c.Collect(ch)
				close(ch)
			}
		}()
	}
	wg.Wait()
}

func TestCollect_DownWhenSysClassMissing(t *testing.T) {
	c := &Collector{sysClassNVMe: "/nonexistent/nvme", devDir: "/dev",
		read: func(string) ([]byte, error) { return nil, nil }}
	got := gather(t, c)
	if got["thermalscope_nvmehealth_up"] != 0 {
		t.Errorf("up = %v, want 0 when sysClassNVMe is unreadable", got["thermalscope_nvmehealth_up"])
	}
}

func TestU128_HighWord(t *testing.T) {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:8], 5)
	binary.LittleEndian.PutUint64(b[8:16], 1) // 2^64 + 5
	got := u128(b)
	want := 18446744073709551616.0 + 5
	if got != want {
		t.Errorf("u128 = %v, want %v", got, want)
	}
}
