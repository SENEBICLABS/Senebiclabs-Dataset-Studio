// Package api assembles the studio-api HTTP surface: it wires the services,
// lists every route in one place and builds the handler. studio-api and the
// route × role matrix test (plan 14, 17.3 #1) both use it, so the test sees
// exactly the routes the binary serves.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/authz"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/httpx"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals/admin"
)

// Recorder takes denied and failed events out of band (the audit
// Aggregator, plan 8.3).
type Recorder interface {
	Add(ev audit.Event) error
}

// Config configures the database-backed routes.
type Config struct {
	// HMACKey keys identifier HMACs for anonymous failed logins.
	HMACKey []byte
	// IdleTimeout ends sessions unused for this long.
	IdleTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Routes wires the database-backed services on pool (connected as
// studio_app) and returns their routes. rec receives failed logins and
// denied requests.
func Routes(pool *pgxpool.Pool, rec Recorder, log *slog.Logger, cfg Config) []httpx.Route {
	authSvc := auth.NewService(pool, auth.Config{
		HMACKey: cfg.HMACKey, IdleTimeout: cfg.IdleTimeout, Failures: rec, Now: cfg.Now,
	})
	authH := auth.NewHandler(authSvc, log)
	adminH := admin.NewHandler(admin.NewService(pool, authz.NewAuthorizer(rec), authSvc, cfg.Now), log, authH.RequireAuth)

	var routes []httpx.Route
	routes = append(routes, authH.Routes()...)
	routes = append(routes, adminH.Routes()...)
	return routes
}

// Handler mounts routes, answers anything else with a 404 problem, and wraps
// the result in the standard middleware (request id, access log, recover).
func Handler(log *slog.Logger, routes []httpx.Route) (http.Handler, error) {
	mux := http.NewServeMux()
	if err := httpx.Mount(mux, routes); err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	mux.Handle("/", httpx.NotFound())
	return httpx.Chain(mux,
		httpx.RequestID(),
		httpx.AccessLog(log),
		httpx.Recover(log),
	), nil
}
