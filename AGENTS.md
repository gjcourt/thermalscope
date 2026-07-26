# thermalscope — Prometheus exporter for host thermal, power & NVMe health metrics

## Overview

thermalscope reads a host's thermal and power sensors and exposes them as
Prometheus metrics on `:9102`. Collectors:

- **hwmon** — CPU temperature (k10temp), NVMe drive temperature + threshold, AMD GPU temperature, and instantaneous power draw, read from `/sys/class/hwmon`.
- **power** — CPU/package energy via RAPL.
- **gpu** — AMD GPU metrics.
- **nvmehealth** — opt-in NVMe SMART/health (wear & failure-prediction). Reading the SMART log needs `CAP_SYS_ADMIN` + `/dev/nvme*`, so it runs as a separate privileged DaemonSet rather than being on by default.

Config is via env: `THERMALSCOPE_HWMON_ROOT` (override the hwmon sysfs root),
`THERMALSCOPE_LOG_LEVEL` (`debug|info|warn|error`), and the NVMe-health opt-in.

## Layout

- `cmd/agent/` — entry point (`main.go`); registers collectors and serves `/metrics`.
- `internal/hwmon/` — hwmon (temperature/power) collector.
- `internal/power/` — RAPL energy collector.
- `internal/gpu/` — AMD GPU collector.
- `internal/nvmehealth/` — NVMe SMART/health collector (opt-in; `ioctl` NVMe admin path).
- `Dockerfile` — distroless static build (`CGO_ENABLED=0`, amd64).

## Develop

Go 1.23. Common tasks (see `Makefile`):

- `make test` — `go test ./...`
- `make tidy` — `go mod tidy`
- `make build` — `docker buildx build --platform=linux/amd64 --load -t ghcr.io/gjcourt/thermalscope:dev .`
- `gofmt -l .` and `go vet ./...` — CI enforces both (see below)

CI (`.github/workflows/build.yml`) runs gofmt, `go vet`, `go test`, and a
`go mod tidy` diff check on every pull request and on push to `main`.

## Container image & deploy

Built and pushed to `ghcr.io/gjcourt/thermalscope` by
`.github/workflows/build.yml` on push to `main` and via `workflow_dispatch`
(GHCR login uses `${{ secrets.GITHUB_TOKEN }}`). Tags emitted additively:

- `main` — branch tag (moving)
- `<sha7>` — short commit sha
- `YYYY-MM-DD` — date tag
- `YYYY-MM-DD-<sha7>` — immutable pin tag (use this to pin a deploy)
- `latest`

Deployed in the homelab via GitOps. The same image serves two workloads, both
pinned by tag + digest:

- `homelab/apps/base/thermalscope/daemonset.yaml` — base thermal/power exporter.
- `homelab/apps/base/thermalscope-smart/daemonset.yaml` — privileged NVMe-health variant.

Bump the pins there — do not repoint `latest`.

## Conventions

- All changes go through a branch and a pull request; never commit directly to
  `main`. Get the PR reviewed and let CI pass before merge.
