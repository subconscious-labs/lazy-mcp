package main

import (
	"context"
	"errors"
	"io"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/catalog"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
	"github.com/nikita-popov/lazy-mcp/internal/mcp"
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
		"prefix", cfg.Proxy.ToolPrefix)

	brok := broker.New(cfg, log)
	cat := catalog.New(cfg, brok, log)
	srv := mcp.NewServer(cfg, brok, cat, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Proxy.Warmup {
		if err := cat.WarmUp(ctx); err != nil {
			log.Warn("warmup error", "err", err)
		}
	}

	brok.StartIdleReaper(ctx)

	log.Info("ready", "transport", "stdio")

	// Run the server on its own goroutine so main can react to shutdown.
	runErr := make(chan error, 1)
	go func() {
		runErr <- srv.Run(ctx, os.Stdin, os.Stdout)
	}()

	exitCode := 0

	select {
	case err := <-runErr:
		// Server exited on its own (EOF on stdin, encode error, etc.).
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("server error", "err", err)
			exitCode = 1
		} else {
			log.Info("server exited")
		}

	case <-ctx.Done():
		// Signal received. Calling stop() restores default signal handling,
		// so a second SIGINT/SIGTERM kills us immediately.
		stop()

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
