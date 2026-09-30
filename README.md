<!-- readme-type: exporter -->
# thermalscope

Prometheus thermal metrics exporter for hestia — CPU, NVMe, and GPU temps via hwmon + nvidia-smi

Per-node CPU, GPU, NVMe, and RAPL power sensors live in scattered sysfs paths
and vendor tools, with no single view of a node's thermal and power state.
thermalscope reads them directly from `sysfs` (hwmon, powercap) and
`/dev/nvme*` — `nvidia-smi` is needed only for NVIDIA GPU metrics — and
exposes them as Prometheus metrics on `:9102`. Each collector degrades
independently: a missing sensor tree or absent `nvidia-smi` turns into a
`*_up 0` gauge for that collector rather than a failed scrape or a crashed
process.

**Status:** in daily use on the homelab since 2026-05, deployed as two
DaemonSets (`thermalscope`, `thermalscope-smart`) in
[`apps/production/thermalscope`](https://github.com/gjcourt/homelab/tree/master/apps/production/thermalscope)
in `gjcourt/homelab`, currently pinned to `2026-07-26-eeaceb6`.

```text
$ THERMALSCOPE_LISTEN_ADDR=:19102 ./thermalscope-agent &
time=2026-09-30T06:11:27.876Z level=INFO msg="metrics server listening" addr=:19102
time=2026-09-30T06:11:28.861Z level=WARN msg="gpu: nvidia-smi unavailable" err="exec: \"nvidia-smi\": executable file not found in $PATH"

$ curl -s localhost:19102/metrics | grep ^thermalscope_
thermalscope_amdgpu_temperature_celsius{chip_index="hwmon2",sensor="edge"} 55
thermalscope_cpu_temperature_celsius{sensor="Tctl"} 67.875
thermalscope_gpu_up 0
thermalscope_hwmon_up 1
thermalscope_nvme_temperature_celsius{device="nvme0",sensor="Composite"} 58.85
thermalscope_nvme_temperature_threshold_celsius{device="nvme0",level="crit",sensor="Composite"} 87.85
thermalscope_nvme_temperature_threshold_celsius{device="nvme0",level="max",sensor="Composite"} 83.85
thermalscope_power_up 1
thermalscope_power_watts{chip="amdgpu"} 11
```

## Why

Is a node running hot or throttling, and is it burning power it doesn't need
to? thermalscope answers that per node, per scrape: CPU/NVMe/GPU temperatures
against their hardware thresholds, instantaneous chip power draw, and
cumulative RAPL package energy, so Prometheus can alert on thermal headroom
before throttling hits and track energy cost per node over time.

## Metrics

### hwmon (`internal/hwmon`, reads `/sys/class/hwmon`)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `thermalscope_cpu_temperature_celsius` | gauge | `sensor` | CPU temperature, read from the k10temp hwmon driver |
| `thermalscope_nvme_temperature_celsius` | gauge | `device`, `sensor` | NVMe drive temperature |
| `thermalscope_nvme_temperature_threshold_celsius` | gauge | `device`, `sensor`, `level` (`crit`/`max`) | NVMe temperature threshold at which the drive throttles (`max`) or is critical (`crit`) |
| `thermalscope_amdgpu_temperature_celsius` | gauge | `sensor`, `chip_index` | AMD GPU temperature, read from the amdgpu hwmon driver |
| `thermalscope_power_watts` | gauge | `chip` | Instantaneous power draw, read from an hwmon `power1_input` |
| `thermalscope_hwmon_up` | gauge | – | `1` if the hwmon sysfs root is readable, else `0` |

### power (`internal/power`, reads the RAPL powercap device tree)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `thermalscope_rapl_energy_microjoules_total` | counter | `domain` | Cumulative RAPL energy consumption, made monotonic in software across hardware counter wraparound |
| `thermalscope_power_up` | gauge | – | `1` if the RAPL powercap root is readable, else `0` |

### gpu (`internal/gpu`, shells out to `nvidia-smi`, NVIDIA only)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `thermalscope_gpu_temperature_celsius` | gauge | `gpu`, `name` | GPU temperature |
| `thermalscope_gpu_power_draw_watts` | gauge | `gpu`, `name` | GPU power draw |
| `thermalscope_gpu_sm_utilization_ratio` | gauge | `gpu`, `name` | GPU SM (compute) utilization, 0–1 |
| `thermalscope_gpu_memory_utilization_ratio` | gauge | `gpu`, `name` | GPU memory utilization, 0–1 |
| `thermalscope_gpu_fan_speed_ratio` | gauge | `gpu`, `name` | GPU fan speed, 0–1 |
| `thermalscope_gpu_sm_clock_hz` | gauge | `gpu`, `name` | GPU SM clock frequency |
| `thermalscope_gpu_up` | gauge | – | `1` if `nvidia-smi` is available and its output parsed, else `0` |

AMD GPU temperature is reported by the hwmon collector above, not this one.

### nvmehealth (`internal/nvmehealth`, opt-in, reads the NVMe SMART/Health log page via an admin ioctl on `/dev/nvme*`)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `thermalscope_nvme_percentage_used_ratio` | gauge | `device` | NVMe endurance consumed as a ratio of rated lifetime (0–1; may exceed 1 past rated endurance) |
| `thermalscope_nvme_available_spare_ratio` | gauge | `device` | NVMe available spare capacity as a ratio of the normalized full value, 0–1 |
| `thermalscope_nvme_available_spare_threshold_ratio` | gauge | `device` | Available-spare ratio below which the drive reports a critical warning |
| `thermalscope_nvme_critical_warning` | gauge | `device` | SMART critical-warning bitfield (`0` = healthy; non-zero bits flag spare/temp/reliability/read-only/volatile-memory/persistent-memory faults) |
| `thermalscope_nvme_media_errors_total` | counter | `device` | Media and data-integrity errors detected over the drive's lifetime |
| `thermalscope_nvme_error_log_entries_total` | counter | `device` | Error information log entries accumulated over the drive's lifetime |
| `thermalscope_nvme_power_on_hours_total` | counter | `device` | Cumulative power-on time, in hours |
| `thermalscope_nvme_power_cycles_total` | counter | `device` | Power-cycle count over the drive's lifetime |
| `thermalscope_nvme_unsafe_shutdowns_total` | counter | `device` | Unsafe-shutdown count over the drive's lifetime |
| `thermalscope_nvme_data_units_written_total` | counter | `device` | Data units written over the drive's lifetime (1 unit = 1000 × 512 bytes per the NVMe spec) |
| `thermalscope_nvme_data_units_read_total` | counter | `device` | Data units read over the drive's lifetime (1 unit = 1000 × 512 bytes per the NVMe spec) |
| `thermalscope_nvme_read_errors_total` | counter | `device` | Failed or short SMART log reads for this device, counted rather than silently dropped |
| `thermalscope_nvmehealth_up` | gauge | – | `1` if the NVMe class directory is readable, else `0` |

Plus the standard Go and process collectors (`go_*`, `process_*`).

The query that answers the Why — NVMe thermal headroom before throttling:

```promql
thermalscope_nvme_temperature_threshold_celsius{level="max"} - on(device, sensor) thermalscope_nvme_temperature_celsius
```

## Quick start

Needs Go 1.23 and a Linux host exposing `/sys/class/hwmon`
(`nvidia-smi` only if there's an NVIDIA GPU).

```bash
git clone https://github.com/gjcourt/thermalscope && cd thermalscope
go run ./cmd/agent
```

```bash
curl -s localhost:9102/metrics | grep ^thermalscope_
```

## Configuration

All configuration is via environment variables; there is no config file.

| Variable | Default | Meaning |
| --- | --- | --- |
| `THERMALSCOPE_LISTEN_ADDR` | `:9102` | HTTP listen address for `/metrics` and `/healthz` |
| `THERMALSCOPE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `THERMALSCOPE_HWMON_ROOT` | `/sys/class/hwmon` | hwmon sysfs root |
| `THERMALSCOPE_POWERCAP_ROOT` | `/sys/devices/virtual/powercap` | RAPL powercap device tree root |
| `THERMALSCOPE_NVME_HEALTH` | off | Set to `1`/`true`/`yes`/`on` to enable the nvmehealth collector |
| `THERMALSCOPE_NVME_SYSCLASS_ROOT` | `/sys/class/nvme` | NVMe controller enumeration root (nvmehealth) |
| `THERMALSCOPE_NVME_DEV_ROOT` | `/dev` | Root under which NVMe char devices are opened (nvmehealth) |

The `*_ROOT` overrides exist so tests can substitute a fake `/sys` tree, and so
a real deployment can point at a bind-mounted path when the container runtime
masks the real one. nvmehealth is off by default: reading the SMART log needs
`CAP_SYS_ADMIN` and access to `/dev/nvme*`, so it runs in a separate,
privileged deployment rather than raising the privilege of the default agent.

## How it works

Collectors are pull-based: nothing is read until Prometheus scrapes
`/metrics`. Each registered `prometheus.Collector` reads its sensors live on
every scrape — there's no background polling loop and no cached sample — and
exposes its own `_up` gauge so one broken subsystem (no RAPL, no
`nvidia-smi`) never blanks the others. See [`ARCHITECTURE.md`](ARCHITECTURE.md)
for the full component/data-flow walkthrough, sysfs paths, and design
decisions.

## Development

```bash
make test   # go test ./...
make tidy   # go mod tidy
gofmt -l .
go vet ./...
```

CI (`.github/workflows/build.yml`) runs `gofmt -l`, `go vet`, `go test`, and a
`go mod tidy` diff check on every pull request and on push to `main`, then
builds the image. See [`AGENTS.md`](AGENTS.md) for repo conventions.

## Deployment

CI builds and pushes the image to `ghcr.io/gjcourt/thermalscope` on push to
`main`, tagged additively as `main`, `<sha7>`, `YYYY-MM-DD`, the immutable
`YYYY-MM-DD-<sha7>` pin, and `latest`. The homelab runs the same image as two
DaemonSets pinned by tag and digest: the default thermal/power agent in
[`apps/base/thermalscope`](https://github.com/gjcourt/homelab/blob/master/apps/base/thermalscope/daemonset.yaml),
and the privileged NVMe-health variant in
[`apps/base/thermalscope-smart`](https://github.com/gjcourt/homelab/blob/master/apps/base/thermalscope-smart/daemonset.yaml).
Bump the pins there — never repoint `latest`.

## License

No licence file yet.
