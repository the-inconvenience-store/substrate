package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/substrate/substrate/internal/api"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/config"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.Load(os.Args[1:], os.Getenv)
	ctx := context.Background()

	dsn := cfg.DatabaseURL
	if cfg.Embedded {
		ep := embeddedpostgres.NewDatabase(
			embeddedpostgres.DefaultConfig().Database("substrate").Port(5433))
		if err := ep.Start(); err != nil {
			logger.Error("start embedded postgres", "err", err)
			os.Exit(1)
		}
		defer func() { _ = ep.Stop() }()
		dsn = "postgres://postgres:postgres@localhost:5433/substrate?sslmode=disable"
	}
	if dsn == "" {
		logger.Error("no database configured: set --database-url or --embedded")
		os.Exit(1)
	}

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		logger.Error("open db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		logger.Error("migrate", "err", err)
		os.Exit(1)
	}

	router := api.NewRouter(api.Deps{
		Workspaces:  workspace.New(pool),
		Collections: collection.New(pool),
		Records:     record.New(pool),
		AdminToken:  cfg.AdminToken,
	})

	logger.Info("substrate starting", "addr", cfg.Addr, "embedded", cfg.Embedded)
	if err := http.ListenAndServe(cfg.Addr, router); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}
