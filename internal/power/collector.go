package power

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// defaultPowercapRoot is the real RAPL device tree, not the /sys/class/powercap
// view. The class dir is a set of relative symlinks
// (intel-rapl:0 -> ../../devices/virtual/powercap/intel-rapl/intel-rapl:0) that
// resolve through /sys/devices/virtual/powercap. Inside our DaemonSet the
// container runtime masks /sys/devices/virtual/powercap with an empty read-only
// tmpfs (containerd's default sysfs masking), so following those symlinks lands
// in the empty tmpfs and every read returns ENOENT -> the collector saw empty
// files and emitted zero series even though power_up was 1.
//
// The device tree itself contains real directories (no symlinks), so we walk it
// directly. In production it is bind-mounted at a non-/sys path
// (THERMALSCOPE_POWERCAP_ROOT) which the runtime does not mask. See the
// DaemonSet manifest.
const defaultPowercapRoot = "/sys/devices/virtual/powercap"

// We identify a RAPL domain by the presence of both a name file and an
// energy_uj counter in the same directory, rather than by matching directory
// name prefixes. This is layout-agnostic: it handles the nested device tree
// (intel-rapl/intel-rapl:0/intel-rapl:0:0/...) and the flat class view alike,
// and naturally skips the intel-rapl-mmio mirror only if it lacks energy_uj
// (it does not), so we additionally read max_energy_range_uj presence is not
// required. To avoid double-counting the mmio mirror, which exposes the same
// energy under a *-mmio name, we de-duplicate by the value of the name file.
const (
	fileName     = "name"
	fileEnergy   = "energy_uj"
	fileMaxRange = "max_energy_range_uj"
)

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
			"0 means the powercap root is unreadable (RAPL is root-gated).",
		nil, nil,
	)
)

// domainState holds the per-domain bookkeeping needed to expose a monotonic
// energy counter on top of the kernel's wrapping energy_uj register.
type domainState struct {
	lastRaw float64 // previous raw energy_uj reading
	offset  float64 // accumulated wraparound offset (sum of max_energy_range_uj)
}

// Collector reads Intel RAPL energy counters from the powercap device tree.
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
	domains, err := c.discover()
	if err != nil {
		slog.Warn("power: cannot read powercap root", "path", c.powercapRoot, "err", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, d := range domains {
		value := c.monotonic(d.name, d.raw, d.maxRange)
		ch <- prometheus.MustNewConstMetric(descEnergy, prometheus.CounterValue, value, d.name)
	}

	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1)
}

// discoveredDomain is a discovered, readable RAPL domain.
type discoveredDomain struct {
	name     string
	raw      float64
	maxRange float64
}

// discover walks the powercap root and returns every directory that exposes a
// readable name + energy_uj pair. It returns an error only if the root itself
// is unreadable (which is what power_up=0 should mean); individual unreadable
// domains are logged at Debug and skipped. Domains are de-duplicated by their
// name file contents so the intel-rapl-mmio mirror (a duplicate "package-0"
// counter) does not double-count.
func (c *Collector) discover() ([]discoveredDomain, error) {
	// Confirm the root is readable up front so an unreadable root maps to
	// power_up=0 rather than an empty-but-up result.
	if _, err := os.ReadDir(c.powercapRoot); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var out []discoveredDomain

	walkErr := filepath.WalkDir(c.powercapRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission or transient errors on a subtree shouldn't abort the
			// whole walk; log and continue.
			slog.Debug("power: walk error", "path", path, "err", err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		energyPath := filepath.Join(path, fileEnergy)
		if _, statErr := os.Stat(energyPath); statErr != nil {
			return nil // not a RAPL domain dir
		}

		name := readTrimmed(path, fileName)
		if name == "" {
			slog.Debug("power: domain has no name file", "dir", path)
			return nil
		}
		if _, dup := seen[name]; dup {
			slog.Debug("power: duplicate domain name, skipping", "name", name, "dir", path)
			return nil
		}

		raw, rerr := readUint(path, fileEnergy)
		if rerr != nil {
			slog.Debug("power: read energy failed", "domain", name, "err", rerr)
			return nil
		}

		maxRange, merr := readUint(path, fileMaxRange)
		if merr != nil {
			// Without the wrap ceiling we can't detect wraparound; fall back to
			// treating the raw value as-is (rate() still tolerates resets).
			slog.Debug("power: max_energy_range_uj unavailable", "domain", name, "err", merr)
			maxRange = 0
		}

		seen[name] = struct{}{}
		out = append(out, discoveredDomain{name: name, raw: raw, maxRange: maxRange})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
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

func readUint(dir, file string) (float64, error) {
	raw := readTrimmed(dir, file)
	if raw == "" {
		return 0, errEmpty
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func readTrimmed(dir, file string) string {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
