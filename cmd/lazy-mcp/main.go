package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if cfg.Proxy.Warmup {
		if err := cat.WarmUp(ctx); err != nil {
			log.Warn("warmup error", "err", err)
		}
	}

	log.Info("ready", "transport", "stdio")

	if err := srv.Run(ctx, os.Stdin, os.Stdout); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}
