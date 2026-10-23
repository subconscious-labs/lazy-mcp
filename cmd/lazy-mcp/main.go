package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/subconscious-labs/lazy-mcp/internal/broker"
	"github.com/subconscious-labs/lazy-mcp/internal/catalog"
	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/hub"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/mcp"
	"github.com/subconscious-labs/lazy-mcp/internal/observability"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazy-mcp: config: %v\n", err)
		os.Exit(1)
	}

	var logSink logx.Sink
	var logCloser io.Closer
	if cfg.Proxy.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Proxy.LogFile), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "lazy-mcp: create log dir: %v\n", err)
			os.Exit(1)
		}
		logSink = &lumberjack.Logger{
			Filename:   cfg.Proxy.LogFile,
			MaxSize:    cfg.Proxy.LogFileMaxMB,
			MaxBackups: cfg.Proxy.LogFileMaxBackups,
			MaxAge:     cfg.Proxy.LogFileMaxAgeDays,
			Compress:   cfg.Proxy.LogFileCompress,
		}
	}
	log, logCloser := logx.NewWithFile(os.Stderr, logSink)
	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}

	var disabled []string
	for _, s := range cfg.Servers {
		if !s.Enabled {
			disabled = append(disabled, s.Name)
		}
	}
	log.Info("config loaded",
		"servers", len(cfg.Servers),
		"enabled", len(cfg.EnabledServers()),
		"disabled", disabled,
		"prefix", cfg.Proxy.ToolPrefix,
		"max_result_bytes", cfg.Proxy.MaxResultBytes,
		"max_arguments_bytes", cfg.Proxy.MaxArgumentsBytes,
		"default_page_size", cfg.Proxy.DefaultPageSize,
		"exec_timeout", cfg.Proxy.ExecTimeout.String(),
		"idle_ttl", cfg.Proxy.IdleTTL.String(),
		"tool_cache_ttl", cfg.Proxy.ToolCacheTTL.String(),
		"initialize_timeout", cfg.Proxy.InitializeTimeout.String(),
		"shutdown_timeout", cfg.Proxy.ShutdownTimeout.String(),
		"version", observability.Version,
	)

	holder := config.NewHolder(cfg)

	brok := broker.New(holder, log)
	cat := catalog.New(holder, brok, log)
	srv := mcp.NewServer(holder, brok, cat, log)

	var hubSrv *hub.Hub
	if cfg.Proxy.HTTP.Addr != "" {
		hubSrv = hub.New(holder, srv, log)
		brok.SetNotificationHandler(func(server, method string, params json.RawMessage) {
			if method == "notifications/tools/list_changed" {
				cat.InvalidateServer(server)
			}
			hubSrv.OnDownstreamNotification(server, method, params)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.Proxy.Warmup {
		if err := cat.WarmUp(ctx); err != nil {
			log.Warn("warmup error", "err", err)
		}
	}

	brok.StartIdleReaper(ctx)

	if cfg.Proxy.MetricsFile != "" {
		col := observability.Default()
		path := cfg.Proxy.MetricsFile
		interval := cfg.Proxy.MetricsInterval
		go func() {
			write := func() {
				if err := col.WriteFile(path); err != nil {
					log.Warn("metrics write failed", "path", path, "err", err)
				}
			}
			write()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					write()
				}
			}
		}()
		log.Info("metrics writer started", "path", path, "interval", interval.String())
	}

	// Signal handling: SIGINT/SIGTERM trigger graceful shutdown;
	// SIGHUP reloads the config in place.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGHUP:
					reload(*cfgPath, holder, brok, cat, log)
				case syscall.SIGINT, syscall.SIGTERM:
					// Restore default handling so a second signal kills.
					signal.Stop(sigCh)
					log.Info("shutdown signal received", "sig", sig.String())
					cancel()
					return
				}
			}
		}
	}()

	log.Info("ready", "transport", "stdio")

	runErr := make(chan error, 1)
	go func() {
		runErr <- srv.Run(ctx, os.Stdin, os.Stdout)
	}()

	if hubSrv != nil {
		go func() {
			if err := hubSrv.Run(ctx); err != nil {
				log.Error("hub error", "err", err)
				cancel()
			}
		}()
		log.Info("http hub enabled", "addr", cfg.Proxy.HTTP.Addr)
	}

	exitCode := 0

	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("server error", "err", err)
			exitCode = 1
		} else {
			log.Info("server exited")
		}

	case <-ctx.Done():
		grace := cfg.Proxy.ShutdownTimeout
		log.Info("shutdown: draining in-flight work", "grace", grace.String())

		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("server error during drain", "err", err)
			}
			log.Info("shutdown: drained cleanly")
		case <-time.After(grace):
			log.Warn("shutdown: grace period expired, forcing exit")
		}
	}

	log.Info("stopping backends")
	brok.StopAll()

	os.Exit(exitCode)
}

// reload loads cfgPath and applies the new config in place.
func reload(cfgPath string, holder *config.Holder, brok *broker.Broker, cat *catalog.Catalog, log *logx.Logger) {
	log.Info("SIGHUP: reloading config", "path", cfgPath)

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("SIGHUP: reload failed", "err", err)
		return
	}

	old := holder.Swap(newCfg)
	brok.ApplyConfig(old, newCfg)
	cat.ApplyConfig(newCfg)

	// Warn about changes that require a full restart.
	if newCfg.Proxy.LogFile != old.Proxy.LogFile {
		log.Warn("SIGHUP: log_file changed; restart required")
	}
	if newCfg.Proxy.MetricsFile != old.Proxy.MetricsFile ||
		newCfg.Proxy.MetricsInterval != old.Proxy.MetricsInterval {
		log.Warn("SIGHUP: metrics config changed; restart required")
	}
	if newCfg.Proxy.CursorTTL != old.Proxy.CursorTTL {
		log.Warn("SIGHUP: cursor_ttl changed; existing PageStore keeps its old TTL")
	}

	log.Info("SIGHUP: config applied",
		"servers", len(newCfg.Servers),
		"enabled", len(newCfg.EnabledServers()))
}
