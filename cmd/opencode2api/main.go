// Command opencode2api starts the API gateway and its management interface.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	adminui "opencode2api/internal/admin"
	"opencode2api/internal/buildinfo"
	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/telemetry"
)

// version remains the linker injection point used by release builds.
var version = "dev"

func main() {
	buildinfo.Version = version
	configPath := flag.String("config", "config.json", "path to config.json")
	listen := flag.String("listen", "", "override the configured API listen address")
	webListen := flag.String("web-listen", "", "override the configured WebUI listen address")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *webListen != "" {
		cfg.WebUI.Listen = *webListen
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	level := new(slog.LevelVar)
	telemetry.SetLogLevel(level, cfg.Logging.Level)
	hub := telemetry.NewLogHub(cfg.Logging.RingSize)
	redactor := config.NewSecretRedactor()
	redactor.Replace(cfg)
	logger := telemetry.NewStructuredLogger(level, hub, redactor)
	monitor := telemetry.NewMonitor()
	manager, err := gateway.NewRuntimeManager(ctx, *configPath, cfg, logger, monitor, hub, redactor, level)
	if err != nil {
		logger.Error("failed to initialize runtime", "component", "runtime", "event", "runtime_initialization_failed", "error", err)
		os.Exit(1)
	}
	defer manager.Shutdown()

	// Fail fast on a misconfigured listen address instead of discovering it
	// in ListenAndServe after the runtime is already up.
	if _, err := net.ResolveTCPAddr("tcp", cfg.Listen); err != nil {
		logger.Error("invalid listen address", "component", "server", "event", "invalid_listen_address", "address", cfg.Listen, "error", err)
		os.Exit(1)
	}
	if cfg.WebUI.Enabled {
		if _, err := net.ResolveTCPAddr("tcp", cfg.WebUI.Listen); err != nil {
			logger.Error("invalid webui listen address", "component", "server", "event", "invalid_listen_address", "address", cfg.WebUI.Listen, "error", err)
			os.Exit(1)
		}
	}
	// ReadTimeout bounds slowloris-style request bodies. WriteTimeout stays
	// unset on purpose: SSE/chat streams legitimately stay open for many
	// minutes, and a server-wide write deadline would sever them. Upstream
	// attempt budgets and the shutdown grace period bound handler lifetime.
	apiServer := &http.Server{
		Addr: cfg.Listen, Handler: manager.Handler(), ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	servers := []*http.Server{apiServer}
	go serveHTTP(cancel, logger, apiServer, "api")

	if cfg.WebUI.Enabled {
		admin := adminui.New(manager, monitor, hub, logger)
		webServer := &http.Server{
			Addr: cfg.WebUI.Listen, Handler: admin.Handler(), ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		}
		servers = append(servers, webServer)
		go serveHTTP(cancel, logger, webServer, "webui")
	}

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "component", "server", "event", "shutdown_failed", "address", server.Addr, "error", err)
		}
	}
}

func serveHTTP(cancel context.CancelFunc, logger *slog.Logger, server *http.Server, component string) {
	logger.Info("server listening", "component", component, "event", "server_started", "address", server.Addr, "version", buildinfo.Version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped unexpectedly", "component", component, "event", "server_failed", "address", server.Addr, "error", err)
		cancel()
	}
}
