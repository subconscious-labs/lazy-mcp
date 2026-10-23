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
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
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

	for _, w := range config.DrainWarnings() {
		log.Warn("config: " + w)
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
	pol := policy.New(holder)
	brok := broker.New(holder, pol, log)
	cat := catalog.New(holder, brok, pol, log)
	srv := mcp.NewServer(holder, brok, cat, pol, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Resolve transport.
	transport := cfg.Proxy.Transport
	if transport == "" {
		if cfg.Proxy.HTTP.Addr != "" {
			transport = "http"
		} else {
			transport = "stdio"
		}
	}
	runStdio := transport == "stdio" || transport == "both"
	runHTTP := transport == "http" || transport == "both"

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

	// HTTP hub.
	var hubSrv *hub.Hub
	var hubErrCh chan error
	if runHTTP {
		hubSrv = hub.New(holder, srv, log)
		brok.SetNotificationHandler(func(server, method string, params json.RawMessage) {
			if method == "notifications/tools/list_changed" {
				cat.InvalidateServer(server)
			}
			hubSrv.OnDownstreamNotification(server, method, params)
		})
	}

	// Signals (SIGHUP reload + INT/TERM shutdown) — как было.
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
					reload(*cfgPath, holder, pol, brok, cat, log)
				case syscall.SIGINT, syscall.SIGTERM:
					signal.Stop(sigCh)
					log.Info("shutdown signal received", "sig", sig.String())
					cancel()
					return
				}
			}
		}
	}()

	if hubSrv != nil {
		hubErrCh = make(chan error, 1)
		go func() { hubErrCh <- hubSrv.Run(ctx) }()
	}

	var runErr chan error
	if runStdio {
		runErr = make(chan error, 1)
		go func() { runErr <- srv.Run(ctx, os.Stdin, os.Stdout) }()
	}

	log.Info("ready", "transport", transport, "http_addr", cfg.Proxy.HTTP.Addr)

	exitCode := 0

	if runStdio {
		// Legacy path: stdio is the primary transport. Exit when it
		// EOFs (client disconnected) or when ctx is cancelled.
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
				log.Warn("shutdown: grace period expired")
			}
		}
	} else {
		// HTTP-only path: daemon. Exit on SIGTERM/SIGINT or fatal hub error.
		select {
		case err := <-hubErrCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error("hub error", "err", err)
				exitCode = 1
			} else {
				log.Info("hub exited")
			}
		case <-ctx.Done():
			// hub.Run already calls Shutdown on ctx.Done; just wait briefly
			// for its graceful stop.
			select {
			case <-hubErrCh:
			case <-time.After(cfg.Proxy.ShutdownTimeout):
				log.Warn("shutdown: grace period expired")
			}
		}
	}

	log.Info("stopping backends")
	brok.StopAll()
	os.Exit(exitCode)
}

// reload loads cfgPath and applies the new config in place.
func reload(cfgPath string, holder *config.Holder, pol *policy.Policy, brok *broker.Broker, cat *catalog.Catalog, log *logx.Logger) {
	log.Info("SIGHUP: reloading config", "path", cfgPath)

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("SIGHUP: reload failed", "err", err)
		return
	}
	for _, w := range config.DrainWarnings() {
		log.Warn("SIGHUP: config: " + w)
	}
	old := holder.Swap(newCfg)
	pol.Invalidate()
	cat.Invalidate()
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
