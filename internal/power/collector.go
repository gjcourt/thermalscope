package power

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const defaultPowercapRoot = "/sys/class/powercap"

// raplDirRe matches intel-rapl domain directories, including nested
// subdomains. Examples: intel-rapl:0 (package), intel-rapl:0:0 (core),
// intel-rapl:0:1 (uncore), intel-rapl:1 (a second socket).
//
// We deliberately do NOT match the "intel-rapl" class root or the
// "intel-rapl-mmio" mirror, only the colon-suffixed domain dirs.
const raplPrefix = "intel-rapl:"

var (
	descEnergy = prometheus.NewDesc(
		"thermalscope_rapl_energy_microjoules_total",
		"Cumulative RAPL energy consumption in microjoules, made monotonic "+
			"across the hardware counter's wraparound. Use PromQL rate() to "+
			"derive average power in watts.",
		[]string{"domain"}, nil,
	)
	descUp = prometheus.NewDesc(
		"thermalscope_power_up",
		"Whether the RAPL power collector is operational (1=up, 0=down). "+
			"0 means /sys/class/powercap is unreadable (RAPL is root-gated).",
		nil, nil,
	)
)

// domainState holds the per-domain bookkeeping needed to expose a monotonic
// energy counter on top of the kernel's wrapping energy_uj register.
type domainState struct {
	lastRaw float64 // previous raw energy_uj reading
	offset  float64 // accumulated wraparound offset (sum of max_energy_range_uj)
}

// Collector reads Intel RAPL energy counters from /sys/class/powercap.
//
// RAPL exposes energy_uj as a counter that wraps at max_energy_range_uj. We
// keep per-domain state (last raw reading + accumulated offset) and, whenever
// the current raw read is below the previous one, add max_energy_range_uj to
// the offset. The exported value is offset + current_raw, which is a genuine
// monotonic counter. This is what makes long-run energy/cost accounting
// accurate rather than relying on rate() merely tolerating resets — the whole
// point of the feature is accurate cumulative energy.
//
// Because Prometheus scrapes may be concurrent, the state map is mutex-guarded.
type Collector struct {
	powercapRoot string

	mu    sync.Mutex
	state map[string]*domainState
}

func NewCollector(powercapRoot string) *Collector {
	if powercapRoot == "" {
		powercapRoot = defaultPowercapRoot
	}
	return &Collector{
		powercapRoot: powercapRoot,
		state:        make(map[string]*domainState),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descEnergy
	ch <- descUp
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	entries, err := os.ReadDir(c.powercapRoot)
	if err != nil {
		slog.Warn("power: cannot read powercap root", "path", c.powercapRoot, "err", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, raplPrefix) {
			continue
		}
		dir := filepath.Join(c.powercapRoot, name)

		domain := c.readFile(dir, "name")
		if domain == "" {
			slog.Debug("power: domain has no name file", "dir", dir)
			continue
		}

		raw, err := c.readUint(dir, "energy_uj")
		if err != nil {
			slog.Debug("power: read energy failed", "domain", domain, "err", err)
			continue
		}

		maxRange, err := c.readUint(dir, "max_energy_range_uj")
		if err != nil {
			// Without the wrap ceiling we can't detect wraparound; fall back
			// to treating the raw value as-is (rate() still tolerates resets).
			slog.Debug("power: max_energy_range_uj unavailable", "domain", domain, "err", err)
			maxRange = 0
		}

		value := c.monotonic(domain, raw, maxRange)
		ch <- prometheus.MustNewConstMetric(descEnergy, prometheus.CounterValue, value, domain)
	}

	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1)
}

// monotonic returns a monotonically increasing energy value for a domain,
// adding maxRange to the accumulated offset whenever the raw counter wraps.
// Caller must hold c.mu.
func (c *Collector) monotonic(domain string, raw, maxRange float64) float64 {
	st, ok := c.state[domain]
	if !ok {
		st = &domainState{}
		c.state[domain] = st
	}
	// A drop in the raw reading means the hardware counter wrapped. Add the
	// ceiling so the exported value keeps climbing. If maxRange is 0 (file
	// missing) we can't compensate, so the counter simply appears to reset —
	// rate() handles that gracefully.
	if raw < st.lastRaw && maxRange > 0 {
		st.offset += maxRange
	}
	st.lastRaw = raw
	return st.offset + raw
}

var errEmpty = errors.New("empty")

func (c *Collector) readUint(dir, file string) (float64, error) {
	raw := c.readFile(dir, file)
	if raw == "" {
		return 0, errEmpty
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func (c *Collector) readFile(dir, file string) string {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
