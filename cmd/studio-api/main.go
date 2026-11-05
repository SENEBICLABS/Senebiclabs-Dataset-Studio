// Command studio-api serves the Dataset Studio HTTP API.
//
// It serves /healthz and /readyz, and, with a database configured, the
// routes assembled by internal/api (authentication from M3; authorization and
// user, service-account and API-key administration from M4).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/api"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/app"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/health"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/httpx"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/logging"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/version"
)

const component = "studio-api"

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

	var checks []health.Checker
	var routes []httpx.Route
	if cfg.DatabaseURL != "" {
		pool, err := db.Open(ctx, cfg.DatabaseURL, component)
		if err != nil {
			log.Error("database pool", slog.String("error", err.Error()))
			return 1
		}
		lc.OnShutdown("database pool", func(context.Context) error { pool.Close(); return nil })
		checks = append(checks, db.PingChecker{Pool: pool})

		// Denied and failed events are aggregated in memory and flushed per
		// window (plan 8.3). Registered after the pool, so its hook runs
		// before the pool closes and after HTTP has drained.
		agg := audit.NewAggregator(pool, log, audit.Actions, audit.AggregatorConfig{})
		go agg.Run()
		lc.OnShutdown("audit aggregator flush", func(ctx context.Context) error {
			return agg.Shutdown(ctx, true)
		})

		// Authentication (M3) and authorization with user, service-account
		// and API-key administration (M4). Failed logins and denied requests
		// go to the aggregator; the HMAC key is required by config.Load.
		routes = api.Routes(pool, agg, log, api.Config{
			HMACKey: []byte(cfg.AuditHMACKey), IdleTimeout: cfg.SessionIdleTimeout,
		})
	}
	hs := health.New(log, cfg.ReadinessTimeout, checks...)
	lc.OnDrain(hs.SetDraining)

	handler, err := api.Handler(log, append(hs.Routes(), routes...))
	if err != nil {
		log.Error("routes", slog.String("error", err.Error()))
		return 1
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	var lcfg net.ListenConfig
	ln, err := lcfg.Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		log.Error("listen", slog.String("addr", cfg.HTTP.Addr), slog.String("error", err.Error()))
		return 1
	}
	lc.ServeHTTP(srv, ln)

	if err := lc.Run(ctx); err != nil {
		return 1
	}
	return 0
}
