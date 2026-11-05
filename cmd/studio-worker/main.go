// Command studio-worker runs background jobs.
//
// M0 scope: configuration, logging and graceful shutdown only. The job queue
// (River) is wired in M6.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/app"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/logging"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/version"
)

const component = "studio-worker"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.LookupEnv, os.Stderr))
}

func run(ctx context.Context, lookup config.Lookup, logOut io.Writer) int {
	cfg, err := config.Load(lookup)
	if err != nil {
		_, _ = fmt.Fprintf(logOut, "%s: invalid configuration:\n%v\n", component, err)
		return 2
	}
	log, err := logging.New(logOut, cfg.LogLevel, cfg.LogFormat, component)
	if err != nil {
		_, _ = fmt.Fprintf(logOut, "%s: %v\n", component, err)
		return 2
	}
	v := version.Get()
	log.Info("starting",
		slog.String("version", v.Version), slog.String("commit", v.Commit),
		slog.String("go", v.GoVersion), slog.Any("config", cfg.Summary()))

	lc := app.New(log, cfg.ShutdownTimeout)
	if cfg.DatabaseURL != "" {
		pool, err := db.Open(ctx, cfg.DatabaseURL, component)
		if err != nil {
			log.Error("database pool", slog.String("error", err.Error()))
			return 1
		}
		lc.OnShutdown("database pool", func(context.Context) error { pool.Close(); return nil })
	}
	log.Info("worker idle: no job kinds are registered in M0")

	if err := lc.Run(ctx); err != nil {
		return 1
	}
	return 0
}
