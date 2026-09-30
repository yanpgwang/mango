package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yanpgwang/mango/internal/pg"
)

func runMigrate() {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	_ = fs.Parse(os.Args[2:])
	if fs.NArg() != 0 {
		log.Fatal("usage: mango migrate")
	}
	databaseURL := strings.TrimSpace(os.Getenv(envDatabaseURL))
	if databaseURL == "" {
		log.Fatalf("migrate: %s is required", envDatabaseURL)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pg.Pool(ctx, databaseURL)
	if err != nil {
		log.Fatalf("migrate: postgres: %v", err)
	}
	defer pool.Close()
	if err := pg.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Print("migrate: schema is current")
}
