package hwmon

import (
	"fmt"
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
	descReadErrors = prometheus.NewDesc(
		"thermalscope_hwmon_read_errors_total",
		"Total errors reading hwmon sensor files.",
		[]string{"chip", "sensor"}, nil,
	)
	descCollectorUp = prometheus.NewDesc(
		"thermalscope_collector_up",
		"Whether the collector subsystem is operational (1=up, 0=down).",
		[]string{"subsystem"}, nil,
	)
)

// Collector reads temperature sensors from /sys/class/hwmon.
type Collector struct {
	hwmonRoot string
	errors    map[string]float64
}

func NewCollector(hwmonRoot string) *Collector {
	if hwmonRoot == "" {
		hwmonRoot = defaultHwmonRoot
	}
	return &Collector{
		hwmonRoot: hwmonRoot,
		errors:    make(map[string]float64),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descCPUTemp
	ch <- descNVMeTemp
	ch <- descReadErrors
	ch <- descCollectorUp
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.errors = make(map[string]float64)

	entries, err := os.ReadDir(c.hwmonRoot)
	if err != nil {
		slog.Warn("hwmon: cannot read hwmon root", "path", c.hwmonRoot, "err", err)
		ch <- prometheus.MustNewConstMetric(descCollectorUp, prometheus.GaugeValue, 0, "hwmon")
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

	for key, count := range c.errors {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) == 2 {
			ch <- prometheus.MustNewConstMetric(descReadErrors, prometheus.CounterValue, count, parts[0], parts[1])
		}
	}

	ch <- prometheus.MustNewConstMetric(descCollectorUp, prometheus.GaugeValue, 1, "hwmon")
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
			c.errors["k10temp:"+label]++
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
			c.errors[fmt.Sprintf("nvme:%s:%s", device, sensorLabel)]++
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
		return 0, fmt.Errorf("empty")
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v / 1000.0, nil
}

func (c *Collector) readFile(dir, file string) string {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
