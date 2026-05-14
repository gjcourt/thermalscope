package gpu

import (
	"context"
	"encoding/csv"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	descGPUTemp = prometheus.NewDesc(
		"thermalscope_gpu_temperature_celsius",
		"GPU temperature in Celsius.",
		[]string{"gpu", "name"}, nil,
	)
	descGPUPower = prometheus.NewDesc(
		"thermalscope_gpu_power_draw_watts",
		"GPU power draw in watts.",
		[]string{"gpu", "name"}, nil,
	)
	descGPUSMUtil = prometheus.NewDesc(
		"thermalscope_gpu_sm_utilization_ratio",
		"GPU SM utilization as a ratio (0–1).",
		[]string{"gpu", "name"}, nil,
	)
	descGPUMemUtil = prometheus.NewDesc(
		"thermalscope_gpu_memory_utilization_ratio",
		"GPU memory utilization as a ratio (0–1).",
		[]string{"gpu", "name"}, nil,
	)
	descGPUFan = prometheus.NewDesc(
		"thermalscope_gpu_fan_speed_ratio",
		"GPU fan speed as a ratio (0–1).",
		[]string{"gpu", "name"}, nil,
	)
	descGPUClock = prometheus.NewDesc(
		"thermalscope_gpu_sm_clock_hz",
		"GPU SM clock frequency in Hz.",
		[]string{"gpu", "name"}, nil,
	)
	descGPUUp = prometheus.NewDesc(
		"thermalscope_gpu_up",
		"Whether the GPU collector is operational (1=up, 0=down). 0 means nvidia-smi is unavailable.",
		nil, nil,
	)
)

const nvidiaSMIQuery = "index,name,temperature.gpu,power.draw,utilization.sm,utilization.memory,fan.speed,clocks.current.sm"

// Collector runs nvidia-smi on each scrape to collect GPU metrics.
type Collector struct{}

func NewCollector() *Collector { return &Collector{} }

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descGPUTemp
	ch <- descGPUPower
	ch <- descGPUSMUtil
	ch <- descGPUMemUtil
	ch <- descGPUFan
	ch <- descGPUClock
	ch <- descGPUUp
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	rows, err := runNvidiaSMI()
	if err != nil {
		slog.Warn("gpu: nvidia-smi unavailable", "err", err)
		ch <- prometheus.MustNewConstMetric(descGPUUp, prometheus.GaugeValue, 0)
		return
	}

	for _, row := range rows {
		if len(row) < 8 {
			continue
		}
		idx := strings.TrimSpace(row[0])
		name := strings.TrimSpace(row[1])

		if v, ok := parseFloat(row[2]); ok {
			ch <- prometheus.MustNewConstMetric(descGPUTemp, prometheus.GaugeValue, v, idx, name)
		}
		if v, ok := parseFloat(row[3]); ok {
			ch <- prometheus.MustNewConstMetric(descGPUPower, prometheus.GaugeValue, v, idx, name)
		}
		if v, ok := parseFloat(row[4]); ok {
			ch <- prometheus.MustNewConstMetric(descGPUSMUtil, prometheus.GaugeValue, v/100.0, idx, name)
		}
		if v, ok := parseFloat(row[5]); ok {
			ch <- prometheus.MustNewConstMetric(descGPUMemUtil, prometheus.GaugeValue, v/100.0, idx, name)
		}
		if v, ok := parseFloat(row[6]); ok {
			ch <- prometheus.MustNewConstMetric(descGPUFan, prometheus.GaugeValue, v/100.0, idx, name)
		}
		if v, ok := parseFloat(row[7]); ok {
			ch <- prometheus.MustNewConstMetric(descGPUClock, prometheus.GaugeValue, v*1e6, idx, name)
		}
	}

	ch <- prometheus.MustNewConstMetric(descGPUUp, prometheus.GaugeValue, 1)
}

func runNvidiaSMI() ([][]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"nvidia-smi",
		"--query-gpu="+nvidiaSMIQuery,
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, err
	}

	r := csv.NewReader(strings.NewReader(string(out)))
	return r.ReadAll()
}

// parseFloat parses a trimmed field, returning (value, true) or (0, false)
// for [N/A] or unparseable values.
func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "[N/A]" || s == "N/A" || s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
