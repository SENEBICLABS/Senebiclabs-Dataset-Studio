// Package app owns process lifecycle: run until a stop signal, then shut down
// in a fixed order within one deadline.
//
// Shutdown order:
//  1. readiness flips to "draining" (load balancers stop sending traffic);
//  2. the HTTP server stops accepting and drains in-flight requests;
//  3. shutdown hooks run in reverse registration order (last started, first
//     stopped). The audit aggregator's flush is registered here, after HTTP
//     has drained, so it sees every event produced by in-flight requests.
//
// Every step shares the single ShutdownTimeout deadline. Hooks always run,
// even once the deadline has expired; they then receive the expired context.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Hook is a named shutdown step.
type Hook struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Lifecycle coordinates startup and ordered shutdown.
type Lifecycle struct {
	log             *slog.Logger
	shutdownTimeout time.Duration

	mu      sync.Mutex
	hooks   []Hook
	server  *http.Server
	ln      net.Listener
	onDrain func()
}

// New returns a Lifecycle with the given overall shutdown deadline.
func New(log *slog.Logger, shutdownTimeout time.Duration) *Lifecycle {
	return &Lifecycle{log: log, shutdownTimeout: shutdownTimeout}
}

// OnShutdown registers a hook. Hooks run in reverse registration order.
func (l *Lifecycle) OnShutdown(name string, fn func(ctx context.Context) error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hooks = append(l.hooks, Hook{Name: name, Fn: fn})
}

// OnDrain registers the callback that marks the process not ready. It runs
// first during shutdown.
func (l *Lifecycle) OnDrain(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onDrain = fn
}

// ServeHTTP attaches an HTTP server and its listener. Run serves it.
func (l *Lifecycle) ServeHTTP(srv *http.Server, ln net.Listener) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.server, l.ln = srv, ln
}

// Run blocks until ctx is cancelled (normally by a signal) or the HTTP server
// fails, then performs ordered shutdown. It returns nil on a clean stop.
func (l *Lifecycle) Run(ctx context.Context) error {
	l.mu.Lock()
	srv, ln := l.server, l.ln
	l.mu.Unlock()

	serveErr := make(chan error, 1)
	if srv != nil {
		go func() {
			l.log.Info("http server listening", slog.String("addr", ln.Addr().String()))
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
				return
			}
			serveErr <- nil
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		l.log.Info("shutdown requested")
	case err := <-serveErr:
		if err != nil {
			runErr = fmt.Errorf("http server: %w", err)
			l.log.Error("http server failed", slog.String("error", err.Error()))
		}
	}

	shutdownErr := l.shutdown()
	return errors.Join(runErr, shutdownErr)
}

func (l *Lifecycle) shutdown() error {
	// The parent context is already cancelled; the deadline is fresh.
	ctx, cancel := context.WithTimeout(context.Background(), l.shutdownTimeout)
	defer cancel()
	start := time.Now()

	l.mu.Lock()
	onDrain, srv := l.onDrain, l.server
	hooks := append([]Hook(nil), l.hooks...)
	l.mu.Unlock()

	if onDrain != nil {
		onDrain()
	}

	var errs []error
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
			_ = srv.Close()
		}
	}
	// Every hook runs, even after the deadline has expired (M0-F4, plan 8.3):
	// a hook given an expired context must fail fast but can still do its
	// non-blocking fallback, such as logging audit aggregates it could not
	// flush. An overrun is reported as an error.
	for i := len(hooks) - 1; i >= 0; i-- {
		h := hooks[i]
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("hook %s started after the shutdown deadline: %w", h.Name, err))
		}
		if err := h.Fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("hook %s: %w", h.Name, err))
		}
	}

	err := errors.Join(errs...)
	if err != nil {
		l.log.Error("shutdown finished with errors",
			slog.String("error", err.Error()), slog.Duration("duration", time.Since(start)))
	} else {
		l.log.Info("shutdown complete", slog.Duration("duration", time.Since(start)))
	}
	return err
}
