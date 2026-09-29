# thermalscope

A Prometheus exporter for host thermal, power, and NVMe health metrics. It
reads sensors directly from `sysfs` (hwmon, powercap) and `/dev/nvme*`, with
no vendor tooling required except `nvidia-smi` for NVIDIA GPU metrics. It runs
as a per-node DaemonSet in a Kubernetes cluster, exposing `/metrics` on
`:9102`.

Each collector degrades independently: a missing sensor tree or absent
`nvidia-smi` turns into a `*_up 0` gauge for that collector rather than a
failed scrape or a crashed process.

## Metrics

**hwmon** (`internal/hwmon`, reads `/sys/class/hwmon`):

| Metric | Type | Labels |
| --- | --- | --- |
| `thermalscope_cpu_temperature_celsius` | gauge | `sensor` |
| `thermalscope_nvme_temperature_celsius` | gauge | `device`, `sensor` |
| `thermalscope_nvme_temperature_threshold_celsius` | gauge | `device`, `sensor`, `level` (`crit`/`max`) |
| `thermalscope_amdgpu_temperature_celsius` | gauge | `sensor`, `chip_index` |
| `thermalscope_power_watts` | gauge | `chip` |
| `thermalscope_hwmon_up` | gauge | – |

**power** (`internal/power`, reads the RAPL powercap device tree):

| Metric | Type | Labels |
| --- | --- | --- |
| `thermalscope_rapl_energy_microjoules_total` | counter | `domain` |
| `thermalscope_power_up` | gauge | – |

`thermalscope_rapl_energy_microjoules_total` is made monotonic in software:
the collector tracks each domain's last raw reading and adds
`max_energy_range_uj` to an offset whenever the hardware counter wraps, so
`rate()` and long-run energy accounting stay accurate across wraparound.

**gpu** (`internal/gpu`, shells out to `nvidia-smi`, NVIDIA only):

| Metric | Type | Labels |
| --- | --- | --- |
| `thermalscope_gpu_temperature_celsius` | gauge | `gpu`, `name` |
| `thermalscope_gpu_power_draw_watts` | gauge | `gpu`, `name` |
| `thermalscope_gpu_sm_utilization_ratio` | gauge | `gpu`, `name` |
| `thermalscope_gpu_memory_utilization_ratio` | gauge | `gpu`, `name` |
| `thermalscope_gpu_fan_speed_ratio` | gauge | `gpu`, `name` |
| `thermalscope_gpu_sm_clock_hz` | gauge | `gpu`, `name` |
| `thermalscope_gpu_up` | gauge | – |

AMD GPU temperature is reported by the hwmon collector above, not this one.

**nvmehealth** (`internal/nvmehealth`, opt-in, reads the NVMe SMART/Health log
page via an admin ioctl on `/dev/nvme*`):

| Metric | Type | Labels |
| --- | --- | --- |
| `thermalscope_nvme_percentage_used_ratio` | gauge | `device` |
| `thermalscope_nvme_available_spare_ratio` | gauge | `device` |
| `thermalscope_nvme_available_spare_threshold_ratio` | gauge | `device` |
| `thermalscope_nvme_critical_warning` | gauge | `device` |
| `thermalscope_nvme_media_errors_total` | counter | `device` |
| `thermalscope_nvme_error_log_entries_total` | counter | `device` |
| `thermalscope_nvme_power_on_hours_total` | counter | `device` |
| `thermalscope_nvme_power_cycles_total` | counter | `device` |
| `thermalscope_nvme_unsafe_shutdowns_total` | counter | `device` |
| `thermalscope_nvme_data_units_written_total` | counter | `device` |
| `thermalscope_nvme_data_units_read_total` | counter | `device` |
| `thermalscope_nvme_read_errors_total` | counter | `device` |
| `thermalscope_nvmehealth_up` | gauge | – |

A failed or short SMART log read increments `thermalscope_nvme_read_errors_total`
for that device rather than being silently dropped; devices that disappear are
pruned so their series don't freeze.

Plus the standard Go and process collectors (`go_*`, `process_*`).

## Configuration

All configuration is via environment variables; there is no config file.

| Variable | Default | Purpose |
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
masks the real one (see below).

nvmehealth is off by default: reading the SMART log needs `CAP_SYS_ADMIN` and
access to `/dev/nvme*`, so it's meant to run in a separate, privileged
deployment rather than raise the privilege of the default agent.

## Running

```
go run ./cmd/agent
```

or build the container image:

```
make build   # docker buildx build --platform=linux/amd64 --load -t ghcr.io/gjcourt/thermalscope:dev .
```

The image is a two-stage build: a `golang:1.23-bookworm` builder produces a
static (`CGO_ENABLED=0`, `linux/amd64`) binary, copied into
`gcr.io/distroless/static-debian12`. It listens on `9102` and runs as
`USER 0:0` (root is required to read most of the sensors above, but the image
carries no shell or package manager).

To enable the NVMe health collector, run the same image with
`THERMALSCOPE_NVME_HEALTH=1` and `/dev/nvme*` bind-mounted in; this needs a
privileged container, so it's normally deployed as a separate workload from
the default agent. See `ARCHITECTURE.md` for the full deployment topology
(the homelab runs this as two DaemonSets, `thermalscope` and
`thermalscope-smart`, both pinned by tag and digest).

Note that on a system using containerd, `/sys/devices/virtual/powercap` may be
masked by an empty tmpfs inside the container; if RAPL metrics are missing
with `thermalscope_power_up 1`, bind-mount the real device tree elsewhere and
point `THERMALSCOPE_POWERCAP_ROOT` at it.

CI builds and pushes this image to `ghcr.io/gjcourt/thermalscope` on push to
`main`, tagged additively as `main`, `<sha7>`, `YYYY-MM-DD`,
`YYYY-MM-DD-<sha7>`, and `latest`.

## Development

Requires Go 1.23.

```
make test   # go test ./...
make tidy   # go mod tidy
gofmt -l .
go vet ./...
```

CI (`.github/workflows/build.yml`) runs `gofmt -l`, `go vet`, `go test`, and a
`go mod tidy` diff check on every pull request and on push to `main`, then
builds the image.

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for a full walkthrough of the
collectors, runtime flow, sysfs paths, and deployment topology.
