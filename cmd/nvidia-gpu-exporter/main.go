package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/V01d42/nvidia-gpu-exporter/internal/collector"
)

func newHandler(maxRequests int, logger *slog.Logger) (http.Handler, error) {
	ngc, err := collector.NewNvidiaGPUCollector(logger)
	if err != nil {
		return nil, fmt.Errorf("couldn't create collector: %s", err)
	}

	r := prometheus.NewRegistry()
	r.MustRegister(versioncollector.NewCollector("nvidia_gpu_exporter"))
	if err := r.Register(ngc); err != nil {
		return nil, fmt.Errorf("couldn't register nvidia gpu collector: %s", err)
	}

	return promhttp.HandlerFor(
		r,
		promhttp.HandlerOpts{
			ErrorLog:            slog.NewLogLogger(logger.Handler(), slog.LevelError),
			ErrorHandling:       promhttp.ContinueOnError,
			MaxRequestsInFlight: maxRequests,
		},
	), nil
}

func main() {
	var (
		listenAddress = kingpin.Flag(
			"web.listen-address",
			"Address to listen on.",
		).Default(":9432").String()
		metricsPath = kingpin.Flag(
			"web.telemetry-path",
			"Path under which to expose metrics.",
		).Default("/metrics").String()
		maxRequests = kingpin.Flag(
			"web.max-requests",
			"Maximum number of parallel scrape requests. Use 0 to disable.",
		).Default("40").Int()
	)

	promslogConfig := &promslog.Config{}
	flag.AddFlags(kingpin.CommandLine, promslogConfig)
	kingpin.CommandLine.UsageWriter(os.Stdout)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promslog.New(promslogConfig)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metricsHandler, err := newHandler(*maxRequests, logger)
	if err != nil {
		logger.Error("failed to create metrics handler", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, metricsHandler)

	server := &http.Server{
		Addr:    *listenAddress,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server shutdown error", "err", err)
		}
	}()

	logger.Info("starting exporter", "addr", *listenAddress, "metrics_path", *metricsPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}

	logger.Info("exporter stopped")
}
