package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/catalog"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
	"github.com/nikita-popov/lazy-mcp/internal/mcp"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	log := logx.New(os.Stderr)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazy-mcp: config: %v\n", err)
		os.Exit(1)
	}

	log.Info("config loaded", "servers", len(cfg.Servers), "prefix", cfg.Proxy.ToolPrefix)

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
