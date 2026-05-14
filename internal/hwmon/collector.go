package hwmon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const defaultHwmonRoot = "/sys/class/hwmon"

// nvmeNameRe matches a friendly NVMe controller name like "nvme0".
var nvmeNameRe = regexp.MustCompile(`^nvme[0-9]+$`)

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
	descAMDGPUTemp = prometheus.NewDesc(
		"thermalscope_amdgpu_temperature_celsius",
		"AMD GPU temperature in Celsius, read from amdgpu hwmon driver.",
		[]string{"sensor", "chip_index"}, nil,
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
	ch <- descAMDGPUTemp
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
		case "amdgpu":
			c.collectAMDGPU(ch, dir, entry.Name())
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

// collectAMDGPU emits temperature readings from an amdgpu hwmon chip.
//
// The amdgpu driver exposes one or more tempN_input files, each paired with a
// tempN_label (e.g. "edge", "junction", "mem") that is stable across reboots.
// We include the hwmon directory basename as `chip_index` so multiple amdgpu
// chips on the same host (multi-GPU boxes) don't collide on identical labels.
//
// Other amdgpu sensors (freqN, in*, power*) are intentionally out of scope for
// this collector; thermalscope is thermal-only.
func (c *Collector) collectAMDGPU(ch chan<- prometheus.Metric, dir, chipIndex string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Debug("hwmon: cannot read amdgpu dir", "dir", dir, "err", err)
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
			slog.Debug("hwmon: read failed", "chip", "amdgpu", "sensor", label, "err", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(descAMDGPUTemp, prometheus.GaugeValue, val, label, chipIndex)
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

// resolveNVMeDevice resolves the friendly nvmeN name for an NVMe hwmon directory.
//
// Modern kernels (verified on Linux 6.18) link hwmonN/device directly to the
// NVMe controller at /sys/devices/.../nvme/nvmeN, so filepath.Base of the
// resolved symlink is the friendly name. Older kernels may link to the PCI
// parent instead, in which case nvmeN lives under <pci>/nvme/nvmeN. As a final
// safety net we walk /sys/class/nvme/nvme*/hwmon* from the opposite direction.
func (c *Collector) resolveNVMeDevice(hwmonDir string) string {
	fallback := filepath.Base(hwmonDir)

	deviceLink := filepath.Join(hwmonDir, "device")
	target, err := filepath.EvalSymlinks(deviceLink)
	if err == nil {
		// Modern layout: device symlink lands directly on the controller.
		if name := filepath.Base(target); nvmeNameRe.MatchString(name) {
			return name
		}
		// Legacy layout: device symlink lands on the PCI parent; nvmeN sits
		// in a "nvme" subdirectory.
		if matches, _ := filepath.Glob(filepath.Join(target, "nvme", "nvme*")); len(matches) > 0 {
			if name := filepath.Base(matches[0]); nvmeNameRe.MatchString(name) {
				return name
			}
		}
	}

	// Inverse traversal: /sys/class/nvme/nvmeN/hwmonM -> match hwmonM to hwmonDir.
	if name := c.lookupNVMeByInverseHwmon(hwmonDir); name != "" {
		return name
	}

	return fallback
}

// lookupNVMeByInverseHwmon finds the nvmeN whose hwmon child resolves to the
// same path as hwmonDir. It uses the sibling /sys/class/nvme directory derived
// from the configured hwmon root so tests can substitute a fake /sys tree.
func (c *Collector) lookupNVMeByInverseHwmon(hwmonDir string) string {
	nvmeClass := filepath.Join(filepath.Dir(c.hwmonRoot), "nvme")
	wantBase := filepath.Base(hwmonDir)
	wantTarget, _ := filepath.EvalSymlinks(hwmonDir)

	entries, err := os.ReadDir(nvmeClass)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !nvmeNameRe.MatchString(e.Name()) {
			continue
		}
		nvmeDir := filepath.Join(nvmeClass, e.Name())
		matches, _ := filepath.Glob(filepath.Join(nvmeDir, "hwmon*"))
		for _, m := range matches {
			if filepath.Base(m) == wantBase {
				return e.Name()
			}
			if wantTarget != "" {
				if t, err := filepath.EvalSymlinks(m); err == nil && t == wantTarget {
					return e.Name()
				}
			}
		}
	}
	return ""
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
