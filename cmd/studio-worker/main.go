// Command studio-worker runs background jobs (plan 12).
//
// It connects as studio_worker and runs the kinds in jobkinds.Production()
// with River (M6): from M5, blob.sweep and blob.audit over the blob store at
// STUDIO_BLOB_ROOT. Without a database it stays idle (as in M0).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/app"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobkinds"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs"
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
	if cfg.DatabaseURL == "" {
		log.Info("worker idle: no database configured")
	} else {
		if cfg.BlobRoot == "" {
			log.Error("STUDIO_BLOB_ROOT must be set when STUDIO_DATABASE_URL is set")
			return 2
		}
		store, err := fsstore.New(cfg.BlobRoot)
		if err != nil {
			log.Error("blob store", slog.String("error", err.Error()))
			return 1
		}
		maint, err := blob.NewMaintenance(blob.MaintenanceConfig{Store: store})
		if err != nil {
			log.Error("blob maintenance", slog.String("error", err.Error()))
			return 1
		}
		registry, err := jobkinds.Production(jobkinds.Deps{Blob: maint})
		if err != nil {
			log.Error("job registry", slog.String("error", err.Error()))
			return 1
		}
		pool, err := db.Open(ctx, cfg.DatabaseURL, component)
		if err != nil {
			log.Error("database pool", slog.String("error", err.Error()))
			return 1
		}
		// Hooks run in reverse registration order: the runner stops (and its
		// jobs finish or are cancelled) before the pool closes.
		lc.OnShutdown("database pool", func(context.Context) error { pool.Close(); return nil })
		runner, err := jobs.NewRunner(jobs.RunnerConfig{Pool: pool, Registry: registry, Logger: log})
		if err != nil {
			log.Error("job runner", slog.String("error", err.Error()))
			return 1
		}
		// The lifecycle drives a graceful stop through the hook below, so the
		// signal context must not hard-stop River on its own.
		if err := runner.Start(context.WithoutCancel(ctx)); err != nil {
			log.Error("job runner", slog.String("error", err.Error()))
			return 1
		}
		lc.OnShutdown("job runner", func(c context.Context) error { return runner.Shutdown(c, 5*time.Second) })
		log.Info("job runner started", slog.Any("kinds", registry.Kinds()))
	}

	if err := lc.Run(ctx); err != nil {
		return 1
	}
	return 0
}
