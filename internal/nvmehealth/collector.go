// Package nvmehealth collects NVMe SMART/Health wear and failure-prediction
// metrics (percentage used, available spare, media errors, critical warning,
// power-on hours, …).
//
// Unlike the hwmon collector — which reads drive *temperature* from plain
// sysfs — these fields live only in the NVMe SMART/Health log page, which the
// kernel does not surface via sysfs. Reading it requires issuing an NVMe admin
// command (Get Log Page, log id 0x02) to the controller character device
// (/dev/nvmeN). That needs CAP_SYS_ADMIN and access to the device node, so this
// collector is intended to run in a separate, privileged DaemonSet rather than
// alongside the unprivileged hwmon agent.
//
// The ioctl itself is Linux-only and lives in ioctl_linux.go; the parser and
// metric emission here are platform-neutral and unit-tested against a fixture
// log page via the injectable read function.
package nvmehealth

import (
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultSysClassNVMe = "/sys/class/nvme"
	defaultDevDir       = "/dev"
	// smartLogLen is the size of the SMART/Health Information log page
	// (NVMe spec figure: 512 bytes).
	smartLogLen = 512
)

// controllerRE matches an NVMe controller name (nvme0, nvme1, …) but not a
// namespace (nvme0n1), so we open the controller char device for the admin cmd.
var controllerRE = regexp.MustCompile(`^nvme[0-9]+$`)

var (
	descPercentageUsed = prometheus.NewDesc(
		"thermalscope_nvme_percentage_used_ratio",
		"NVMe endurance consumed as a ratio of rated lifetime (0-1; may exceed 1 past the rated endurance).",
		[]string{"device"}, nil,
	)
	descAvailableSpare = prometheus.NewDesc(
		"thermalscope_nvme_available_spare_ratio",
		"NVMe available spare capacity as a ratio of the normalized full value (0-1).",
		[]string{"device"}, nil,
	)
	descAvailableSpareThreshold = prometheus.NewDesc(
		"thermalscope_nvme_available_spare_threshold_ratio",
		"NVMe available-spare threshold below which the drive reports a critical warning (0-1).",
		[]string{"device"}, nil,
	)
	descCriticalWarning = prometheus.NewDesc(
		"thermalscope_nvme_critical_warning",
		"NVMe critical warning bitfield (0 = healthy; non-zero bits flag spare/temp/reliability/read-only/volatile-memory/persistent-memory faults).",
		[]string{"device"}, nil,
	)
	descMediaErrors = prometheus.NewDesc(
		"thermalscope_nvme_media_errors_total",
		"NVMe media and data-integrity errors detected over the drive's lifetime.",
		[]string{"device"}, nil,
	)
	descErrorLogEntries = prometheus.NewDesc(
		"thermalscope_nvme_error_log_entries_total",
		"NVMe error information log entries accumulated over the drive's lifetime.",
		[]string{"device"}, nil,
	)
	descPowerOnHours = prometheus.NewDesc(
		"thermalscope_nvme_power_on_hours_total",
		"NVMe cumulative power-on time in hours.",
		[]string{"device"}, nil,
	)
	descPowerCycles = prometheus.NewDesc(
		"thermalscope_nvme_power_cycles_total",
		"NVMe power-cycle count over the drive's lifetime.",
		[]string{"device"}, nil,
	)
	descUnsafeShutdowns = prometheus.NewDesc(
		"thermalscope_nvme_unsafe_shutdowns_total",
		"NVMe unsafe-shutdown count over the drive's lifetime.",
		[]string{"device"}, nil,
	)
	descDataUnitsWritten = prometheus.NewDesc(
		"thermalscope_nvme_data_units_written_total",
		"NVMe data units written over the drive's lifetime (1 unit = 1000 * 512 bytes per the NVMe spec).",
		[]string{"device"}, nil,
	)
	descDataUnitsRead = prometheus.NewDesc(
		"thermalscope_nvme_data_units_read_total",
		"NVMe data units read over the drive's lifetime (1 unit = 1000 * 512 bytes per the NVMe spec).",
		[]string{"device"}, nil,
	)
	descReadErrors = prometheus.NewDesc(
		"thermalscope_nvme_read_errors_total",
		"Cumulative count of failed NVMe SMART/Health log reads per controller.",
		[]string{"device"}, nil,
	)
	descUp = prometheus.NewDesc(
		"thermalscope_nvmehealth_up",
		"Whether the NVMe health collector is operational (1=up, 0=down).",
		nil, nil,
	)
)

// Collector reads the SMART/Health log page from each NVMe controller.
type Collector struct {
	sysClassNVMe string
	devDir       string
	// read returns the raw SMART/Health log page for the controller device at
	// devPath. Injectable so tests can supply a fixture without a real device.
	read func(devPath string) ([]byte, error)

	// mu guards readErrors and failing. Collect may be called concurrently by
	// the Prometheus registry, and the collector is otherwise stateless.
	mu sync.Mutex
	// readErrors accumulates the per-device count of failed log reads across
	// scrapes, keyed by controller name (e.g. "nvme0").
	readErrors map[string]float64
	// failing tracks whether each device's last read failed, so we log Warn
	// only on the transition into the failing state rather than every scrape.
	failing map[string]bool
}

// NewCollector returns a collector using the host's real NVMe devices.
// sysClassNVMe and devDir default to /sys/class/nvme and /dev when empty.
func NewCollector(sysClassNVMe, devDir string) *Collector {
	if sysClassNVMe == "" {
		sysClassNVMe = defaultSysClassNVMe
	}
	if devDir == "" {
		devDir = defaultDevDir
	}
	return &Collector{
		sysClassNVMe: sysClassNVMe,
		devDir:       devDir,
		read:         readSMARTLog,
		readErrors:   map[string]float64{},
		failing:      map[string]bool{},
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descPercentageUsed
	ch <- descAvailableSpare
	ch <- descAvailableSpareThreshold
	ch <- descCriticalWarning
	ch <- descMediaErrors
	ch <- descErrorLogEntries
	ch <- descPowerOnHours
	ch <- descPowerCycles
	ch <- descUnsafeShutdowns
	ch <- descDataUnitsWritten
	ch <- descDataUnitsRead
	ch <- descReadErrors
	ch <- descUp
}

// deviceResult is the outcome of reading one controller this scrape: either a
// parsed health page (failed=false) or a failed/partial read (failed=true).
type deviceResult struct {
	name   string
	health smartHealth
	failed bool
	// readErr/shortLen describe the failure for transition logging.
	readErr  error
	shortLen int
	short    bool
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	entries, err := os.ReadDir(c.sysClassNVMe)
	if err != nil {
		slog.Warn("nvmehealth: cannot list NVMe controllers", "path", c.sysClassNVMe, "err", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}

	// Phase 1: read every controller WITHOUT holding the mutex. The device
	// read may issue a blocking ioctl, so it must never run under the lock.
	var results []deviceResult
	for _, entry := range entries {
		name := entry.Name()
		if !controllerRE.MatchString(name) {
			continue
		}
		devPath := filepath.Join(c.devDir, name)
		raw, err := c.read(devPath)
		switch {
		case err != nil:
			// A failed admin-ioctl read (e.g. a device-cgroup EPERM denial)
			// must be visible and alertable, not a silent skip.
			results = append(results, deviceResult{name: name, failed: true, readErr: err})
		case len(raw) < smartLogLen:
			// A short/partial log page is a failed read too — count it and
			// surface it the same way rather than silently dropping it.
			results = append(results, deviceResult{name: name, failed: true, short: true, shortLen: len(raw)})
		default:
			results = append(results, deviceResult{name: name, health: parseSMARTLog(raw)})
		}
	}

	// Phase 2: under the lock, update counters and the failing-state map,
	// prune devices no longer present, and snapshot what to log/emit. We do
	// NOT hold the lock across any channel send or device read.
	type errSnapshot struct {
		name  string
		count float64
	}
	var (
		errSnaps     []errSnapshot
		warnOnFailed []deviceResult // devices that just transitioned into failing
		recovered    []string       // devices that just transitioned out of failing
	)

	c.mu.Lock()
	if c.readErrors == nil {
		c.readErrors = map[string]float64{}
	}
	if c.failing == nil {
		c.failing = map[string]bool{}
	}

	present := make(map[string]struct{}, len(results))
	for _, r := range results {
		present[r.name] = struct{}{}
		if r.failed {
			c.readErrors[r.name]++
			if !c.failing[r.name] {
				c.failing[r.name] = true
				warnOnFailed = append(warnOnFailed, r)
			}
		} else {
			// Ensure a baseline so a healthy device reports 0 rather than
			// dropping out of the series.
			if _, seen := c.readErrors[r.name]; !seen {
				c.readErrors[r.name] = 0
			}
			if c.failing[r.name] {
				c.failing[r.name] = false
				recovered = append(recovered, r.name)
			}
		}
	}

	// Prune devices that disappeared this scrape so we stop emitting a frozen
	// series and the maps don't grow without bound.
	for device := range c.readErrors {
		if _, ok := present[device]; !ok {
			delete(c.readErrors, device)
			delete(c.failing, device)
		}
	}
	// failing can hold a device absent from readErrors only transiently; prune
	// any stragglers so the maps stay bounded together.
	for device := range c.failing {
		if _, ok := present[device]; !ok {
			delete(c.failing, device)
		}
	}

	// Snapshot the counts for currently-present devices to emit after unlock.
	for device := range present {
		errSnaps = append(errSnaps, errSnapshot{name: device, count: c.readErrors[device]})
	}
	c.mu.Unlock()

	// Log transitions outside the lock. Warn only on entry into the failing
	// state; a single Info on recovery.
	for _, r := range warnOnFailed {
		if r.short {
			slog.Warn("nvmehealth: short log page", "device", r.name, "len", r.shortLen)
		} else {
			slog.Warn("nvmehealth: read failed", "device", r.name, "err", r.readErr)
		}
	}
	for _, device := range recovered {
		slog.Info("nvmehealth: read recovered", "device", device)
	}

	// Phase 3: emit all metrics WITHOUT holding the lock.
	for _, r := range results {
		if !r.failed {
			c.emit(ch, r.name, r.health)
		}
	}
	for _, s := range errSnaps {
		ch <- prometheus.MustNewConstMetric(descReadErrors, prometheus.CounterValue, s.count, s.name)
	}
	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1)
}

func (c *Collector) emit(ch chan<- prometheus.Metric, device string, h smartHealth) {
	g := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, device)
	}
	ct := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, device)
	}
	g(descPercentageUsed, h.percentageUsed/100)
	g(descAvailableSpare, h.availableSpare/100)
	g(descAvailableSpareThreshold, h.availableSpareThreshold/100)
	g(descCriticalWarning, h.criticalWarning)
	ct(descMediaErrors, h.mediaErrors)
	ct(descErrorLogEntries, h.errorLogEntries)
	ct(descPowerOnHours, h.powerOnHours)
	ct(descPowerCycles, h.powerCycles)
	ct(descUnsafeShutdowns, h.unsafeShutdowns)
	ct(descDataUnitsWritten, h.dataUnitsWritten)
	ct(descDataUnitsRead, h.dataUnitsRead)
}

// smartHealth holds the parsed fields we expose from the SMART/Health log page.
type smartHealth struct {
	criticalWarning         float64
	availableSpare          float64 // percent
	availableSpareThreshold float64 // percent
	percentageUsed          float64 // percent (may exceed 100)
	dataUnitsRead           float64
	dataUnitsWritten        float64
	powerCycles             float64
	powerOnHours            float64
	unsafeShutdowns         float64
	mediaErrors             float64
	errorLogEntries         float64
}

// parseSMARTLog decodes the NVMe SMART/Health Information log page (log id
// 0x02). Field offsets follow the NVMe base specification; multi-byte counters
// are little-endian 128-bit values. Callers must ensure len(b) >= smartLogLen.
func parseSMARTLog(b []byte) smartHealth {
	return smartHealth{
		criticalWarning:         float64(b[0]),
		availableSpare:          float64(b[3]),
		availableSpareThreshold: float64(b[4]),
		percentageUsed:          float64(b[5]),
		dataUnitsRead:           u128(b[32:48]),
		dataUnitsWritten:        u128(b[48:64]),
		powerCycles:             u128(b[112:128]),
		powerOnHours:            u128(b[128:144]),
		unsafeShutdowns:         u128(b[144:160]),
		mediaErrors:             u128(b[160:176]),
		errorLogEntries:         u128(b[176:192]),
	}
}

// u128 converts a little-endian 128-bit value to float64. These lifetime
// counters never approach 2^64 in practice, but the high word is included so
// the value stays correct if one ever does (with float64's loss of precision
// at that magnitude, which is acceptable for a monitoring counter).
func u128(b []byte) float64 {
	lo := binary.LittleEndian.Uint64(b[0:8])
	hi := binary.LittleEndian.Uint64(b[8:16])
	const twoPow64 = 18446744073709551616.0
	return float64(hi)*twoPow64 + float64(lo)
}
