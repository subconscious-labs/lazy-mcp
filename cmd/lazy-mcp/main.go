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
	"sync"
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

var reloadMutex sync.Mutex

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
	var hubSrv *hub.Hub
	reloadFn := func() ([]string, error) {
		return reload(*cfgPath, holder, pol, brok, cat, hubSrv, log)
	}
	srv := mcp.NewServer(holder, brok, cat, pol, log, reloadFn)

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

	// Autostart: bring up backends that opted in. Lazy servers stay
	// down until the first call; this is for operators who want a
	// warm pool at boot.
	for _, srv := range cfg.EnabledServers() {
		if !srv.Autostart {
			continue
		}
		if _, err := brok.Ensure(ctx, srv.Name); err != nil {
			log.Warn("autostart failed", "server", srv.Name, "err", err)
		} else {
			log.Info("autostart: backend started", "server", srv.Name)
		}
	}

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
	var hubErrCh chan error
	var runErr chan error
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
					warns, err := reload(*cfgPath, holder, pol, brok, cat, hubSrv, log)
					if err != nil {
						log.Error("SIGHUP: reload failed", "err", err)
					}
					for _, w := range warns {
						log.Warn("SIGHUP: " + w)
					}
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
		go func() { hubErrCh <- hubSrv.Run(ctx) }()
	}

	if runStdio {
		runErr = make(chan error, 1)
		go func() { runErr <- srv.Run(ctx, os.Stdin, os.Stdout) }()
	}

	log.Info("ready", "transport", transport, "http_addr", cfg.Proxy.HTTP.Addr)

	exitCode := 0

	select {
	case err := <-hubErrCh:
		// Only reachable when hub is running. Fatal hub error or clean
		// shutdown caused by ctx.Done() from a signal.
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("hub error", "err", err)
			exitCode = 1
		} else {
			log.Info("hub exited")
		}

	case err := <-runErr:
		// Only reachable when stdio is running.
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("stdio server error", "err", err)
			exitCode = 1
		} else {
			log.Info("stdio server exited")
		}

	case <-ctx.Done():
		grace := cfg.Proxy.ShutdownTimeout
		// hub.Run internally does: close SSE -> httpSrv.Shutdown(grace).
		// Wait long enough for that whole sequence, plus a margin, so
		// we never os.Exit while hub still has POSTs in flight.
		waitFor := grace + 2*time.Second
		log.Info("shutdown: draining in-flight work",
			"grace", grace.String(), "wait_for", waitFor.String())

		drainCtx, drainCancel := context.WithTimeout(context.Background(), waitFor)
		defer drainCancel()

		if hubSrv != nil {
			select {
			case <-hubErrCh:
				log.Info("shutdown: hub drained cleanly")
			case <-drainCtx.Done():
				log.Warn("shutdown: grace period expired, forcing exit")
			}
		}
		if runStdio {
			select {
			case <-runErr:
				log.Info("shutdown: stdio drained cleanly")
			case <-drainCtx.Done():
				log.Warn("shutdown: stdio grace period expired")
			}
		}
	}

	log.Info("stopping backends")
	brok.StopAll()
	os.Exit(exitCode)
}

// reload loads cfgPath, applies the new config, and returns any
// operator-facing warnings (duplicate servers, fields that need a
// full restart). Errors mean the config was not applied — the old
// configuration stays live.
func reload(cfgPath string, holder *config.Holder, pol *policy.Policy, brok *broker.Broker, cat *catalog.Catalog, hubSrv *hub.Hub, log *logx.Logger) ([]string, error) {
	reloadMutex.Lock()
	defer reloadMutex.Unlock()

	log.Info("reload: loading config", "path", cfgPath)

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("reload: load failed", "err", err)
		return nil, err
	}

	var warnings []string
	warnings = append(warnings, config.DrainWarnings()...)

	old := holder.Get()

	// Publish the new config first, then reconcile the broker pool,
	// all while Ensure() is blocked on reloadMu.RLock(). Any Ensure
	// waiting for us sees the new config and the reconciled pool;
	// none can start a backend using the old config after we commit.
	holder.Swap(newCfg)
	brok.ApplyConfig(old, newCfg)
	pol.Invalidate()
	cat.Invalidate()
	cat.ApplyConfig(newCfg)

	if hubSrv != nil {
		hubSrv.RefreshSessionsAfterReload()
	}

	if newCfg.Proxy.LogFile != old.Proxy.LogFile {
		warnings = append(warnings, "log_file changed; restart required")
	}
	if newCfg.Proxy.MetricsFile != old.Proxy.MetricsFile ||
		newCfg.Proxy.MetricsInterval != old.Proxy.MetricsInterval {
		warnings = append(warnings, "metrics config changed; restart required")
	}
	if newCfg.Proxy.CursorTTL != old.Proxy.CursorTTL {
		warnings = append(warnings, "cursor_ttl changed; existing PageStore keeps its old TTL")
	}

	log.Info("reload: applied",
		"servers", len(newCfg.Servers),
		"enabled", len(newCfg.EnabledServers()))
	return warnings, nil
}
