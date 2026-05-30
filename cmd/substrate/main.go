package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/substrate/substrate/internal/api"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := ":8080"
	logger.Info("substrate starting", "addr", addr)
	if err := http.ListenAndServe(addr, api.NewRouter(api.Deps{})); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}
