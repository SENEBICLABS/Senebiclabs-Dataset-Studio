package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ok(name string) Checker {
	return CheckFunc{CheckName: name, Fn: func(context.Context) error { return nil }}
}

func get(t *testing.T, s *Service, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body not JSON: %v", path, err)
	}
	return rr.Code, body
}

func TestHealthzAlwaysOK(t *testing.T) {
	s := New(quiet(), time.Second, CheckFunc{CheckName: "db", Fn: func(context.Context) error { return errors.New("down") }})
	if code, _ := get(t, s, "/healthz"); code != 200 {
		t.Errorf("healthz = %d; liveness must not depend on dependencies", code)
	}
}

func TestReadyWhenAllChecksPass(t *testing.T) {
	s := New(quiet(), time.Second, ok("a"), ok("b"))
	code, body := get(t, s, "/readyz")
	if code != 200 || body["status"] != "ready" {
		t.Errorf("readyz = %d %v", code, body)
	}
}

func TestNotReadyWhenACheckFailsAndErrorNotLeaked(t *testing.T) {
	s := New(quiet(), time.Second, ok("a"),
		CheckFunc{CheckName: "database", Fn: func(context.Context) error {
			return errors.New("dial tcp db.internal:5432: password authentication failed")
		}})
	mux := http.NewServeMux()
	s.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rr.Code != 503 {
		t.Fatalf("readyz = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "db.internal") || strings.Contains(rr.Body.String(), "password") {
		t.Errorf("readiness leaked error detail: %s", rr.Body.String())
	}
	var rep Report
	_ = json.Unmarshal(rr.Body.Bytes(), &rep)
	if rep.Checks["database"].Status != "failed" || rep.Checks["a"].Status != "ok" {
		t.Errorf("checks = %+v", rep.Checks)
	}
}

func TestSlowCheckTimesOut(t *testing.T) {
	s := New(quiet(), 50*time.Millisecond, CheckFunc{CheckName: "slow", Fn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	start := time.Now()
	code, _ := get(t, s, "/readyz")
	if code != 503 {
		t.Errorf("readyz = %d", code)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("check timeout not enforced")
	}
}

func TestDrainingIsNotReady(t *testing.T) {
	s := New(quiet(), time.Second, ok("a"))
	s.SetDraining()
	code, body := get(t, s, "/readyz")
	if code != 503 || body["status"] != "draining" {
		t.Errorf("readyz = %d %v", code, body)
	}
}

func TestNoChecksIsReady(t *testing.T) {
	s := New(quiet(), time.Second)
	if code, _ := get(t, s, "/readyz"); code != 200 {
		t.Errorf("readyz with no dependencies = %d", code)
	}
}
