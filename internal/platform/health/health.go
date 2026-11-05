// Package health serves liveness (/healthz) and readiness (/readyz).
//
// Liveness answers "is the process serving HTTP". Readiness answers "should
// traffic be sent here": every registered dependency check must pass and the
// process must not be draining for shutdown.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/httpx"
)

// Checker is one readiness dependency.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckFunc adapts a function to Checker.
type CheckFunc struct {
	CheckName string
	Fn        func(ctx context.Context) error
}

// Name implements Checker.
func (c CheckFunc) Name() string { return c.CheckName }

// Check implements Checker.
func (c CheckFunc) Check(ctx context.Context) error { return c.Fn(ctx) }

// Service holds readiness state.
type Service struct {
	log      *slog.Logger
	timeout  time.Duration
	checkers []Checker
	draining atomic.Bool
}

// New returns a Service whose checks each run under timeout.
func New(log *slog.Logger, timeout time.Duration, checkers ...Checker) *Service {
	return &Service{log: log, timeout: timeout, checkers: checkers}
}

// SetDraining marks the process as shutting down; readiness fails from then on.
func (s *Service) SetDraining() { s.draining.Store(true) }

// CheckResult is the per-dependency outcome. Error detail is logged, not
// returned, so readiness never leaks hostnames or credentials.
type CheckResult struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
}

// Report is the /readyz body.
type Report struct {
	Status string                 `json:"status"`
	Checks map[string]CheckResult `json:"checks"`
}

// Readiness runs every check concurrently and reports the aggregate.
func (s *Service) Readiness(ctx context.Context) Report {
	rep := Report{Status: "ready", Checks: map[string]CheckResult{}}
	if s.draining.Load() {
		rep.Status = "draining"
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, c := range s.checkers {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			start := time.Now()
			err := c.Check(cctx)
			res := CheckResult{Status: "ok", DurationMS: time.Since(start).Milliseconds()}
			if err != nil {
				res.Status = "failed"
				s.log.WarnContext(ctx, "readiness check failed",
					slog.String("check", c.Name()), slog.String("error", err.Error()))
			}
			mu.Lock()
			rep.Checks[c.Name()] = res
			mu.Unlock()
		})
	}
	wg.Wait()
	if rep.Status == "ready" {
		for _, r := range rep.Checks {
			if r.Status != "ok" {
				rep.Status = "not_ready"
				break
			}
		}
	}
	return rep
}

// CheckNames returns the registered check names, sorted.
func (s *Service) CheckNames() []string {
	names := make([]string, 0, len(s.checkers))
	for _, c := range s.checkers {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	return names
}

// Register mounts GET /healthz and GET /readyz on mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		rep := s.Readiness(r.Context())
		status := http.StatusOK
		if rep.Status != "ready" {
			status = http.StatusServiceUnavailable
		}
		httpx.WriteJSON(w, status, rep)
	})
}
