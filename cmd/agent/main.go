package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gjcourt/thermalscope/internal/gpu"
	"github.com/gjcourt/thermalscope/internal/hwmon"
	"github.com/gjcourt/thermalscope/internal/nvmehealth"
	"github.com/gjcourt/thermalscope/internal/power"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(os.Getenv("THERMALSCOPE_LOG_LEVEL")),
	})))
	if err := run(); err != nil {
		slog.Error("thermalscope exiting", "err", err)
		os.Exit(1)
	}
}

// logLevel parses THERMALSCOPE_LOG_LEVEL (debug|info|warn|error, case-insensitive)
// into an slog.Level, defaulting to Info on empty or unrecognized values. This
// is what surfaces the collectors' per-domain Debug diagnostics (e.g. the RAPL
// "read energy failed" / "domain has no name file" lines) in production.
func logLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// enabled reports whether an env var is set to a truthy value (1/true/yes,
// case-insensitive).
func enabled(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func run() error {
	listenAddr := os.Getenv("THERMALSCOPE_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":9102"
	}
	hwmonRoot := os.Getenv("THERMALSCOPE_HWMON_ROOT")
	powercapRoot := os.Getenv("THERMALSCOPE_POWERCAP_ROOT")

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		hwmon.NewCollector(hwmonRoot),
		gpu.NewCollector(),
		power.NewCollector(powercapRoot),
	)

	// NVMe SMART/Health (wear & failure-prediction) is opt-in: reading the
	// SMART log requires CAP_SYS_ADMIN + /dev/nvme* access, so it runs only in
	// the dedicated privileged DaemonSet that sets THERMALSCOPE_NVME_HEALTH=1.
	// The default (unprivileged, sysfs-only) agent leaves it off.
	if enabled(os.Getenv("THERMALSCOPE_NVME_HEALTH")) {
		slog.Info("nvme health collector enabled")
		reg.MustRegister(nvmehealth.NewCollector(
			os.Getenv("THERMALSCOPE_NVME_SYSCLASS_ROOT"),
			os.Getenv("THERMALSCOPE_NVME_DEV_ROOT"),
		))
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("metrics server listening", "addr", listenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("server shutdown", "err", err)
	}
	return nil
}
