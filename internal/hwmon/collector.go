package hwmon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const defaultHwmonRoot = "/sys/class/hwmon"

var (
	descCPUTemp = prometheus.NewDesc(
		"thermalscope_cpu_temperature_celsius",
		"CPU temperature in Celsius, read from k10temp hwmon driver.",
		[]string{"sensor"}, nil,
	)
	descNVMeTemp = prometheus.NewDesc(
		"thermalscope_nvme_temperature_celsius",
		"NVMe drive temperature in Celsius.",
		[]string{"device", "sensor"}, nil,
	)
	descUp = prometheus.NewDesc(
		"thermalscope_hwmon_up",
		"Whether the hwmon collector is operational (1=up, 0=down).",
		nil, nil,
	)
)

// Collector reads temperature sensors from /sys/class/hwmon.
type Collector struct {
	hwmonRoot string
}

func NewCollector(hwmonRoot string) *Collector {
	if hwmonRoot == "" {
		hwmonRoot = defaultHwmonRoot
	}
	return &Collector{hwmonRoot: hwmonRoot}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descCPUTemp
	ch <- descNVMeTemp
	ch <- descUp
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	entries, err := os.ReadDir(c.hwmonRoot)
	if err != nil {
		slog.Warn("hwmon: cannot read hwmon root", "path", c.hwmonRoot, "err", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "hwmon") {
			continue
		}
		dir := filepath.Join(c.hwmonRoot, entry.Name())
		chip := c.readFile(dir, "name")
		if chip == "" {
			continue
		}
		switch chip {
		case "k10temp":
			c.collectK10temp(ch, dir)
		case "nvme":
			c.collectNVMe(ch, dir)
		}
	}

	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1)
}

func (c *Collector) collectK10temp(ch chan<- prometheus.Metric, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_input") || !strings.HasPrefix(name, "temp") {
			continue
		}
		base := strings.TrimSuffix(name, "_input")
		label := c.readFile(dir, base+"_label")
		if label == "" {
			label = base
		}
		val, err := c.readMilliCelsius(dir, name)
		if err != nil {
			slog.Debug("hwmon: read failed", "chip", "k10temp", "sensor", label, "err", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(descCPUTemp, prometheus.GaugeValue, val, label)
	}
}

func (c *Collector) collectNVMe(ch chan<- prometheus.Metric, dir string) {
	device := c.resolveNVMeDevice(dir)

	sensorNames := map[string]string{
		"temp1": "Composite",
		"temp2": "Sensor1",
	}
	for base, sensorLabel := range sensorNames {
		val, err := c.readMilliCelsius(dir, base+"_input")
		if err != nil {
			slog.Debug("hwmon: read failed", "device", device, "sensor", sensorLabel, "err", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(descNVMeTemp, prometheus.GaugeValue, val, device, sensorLabel)
	}
}

// resolveNVMeDevice resolves the friendly nvmeN name from the hwmon symlink.
func (c *Collector) resolveNVMeDevice(hwmonDir string) string {
	// hwmonDir/device is a symlink to the PCI device; walk up to find the nvmeN dir.
	deviceLink := filepath.Join(hwmonDir, "device")
	target, err := filepath.EvalSymlinks(deviceLink)
	if err != nil {
		return filepath.Base(hwmonDir)
	}
	// The nvme block device dir lives at <pci-dev>/nvme/nvmeN
	nvmeGlob := filepath.Join(target, "nvme", "nvme*")
	matches, err := filepath.Glob(nvmeGlob)
	if err != nil || len(matches) == 0 {
		return filepath.Base(hwmonDir)
	}
	return filepath.Base(matches[0])
}

func (c *Collector) readMilliCelsius(dir, file string) (float64, error) {
	raw := c.readFile(dir, file)
	if raw == "" {
		return 0, errEmpty
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v / 1000.0, nil
}

var errEmpty = errors.New("empty")

func (c *Collector) readFile(dir, file string) string {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
