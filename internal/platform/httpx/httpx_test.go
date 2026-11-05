package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratedPropagatedAndSanitised(t *testing.T) {
	var seen string
	h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}), RequestID())

	// Generated when absent.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if len(seen) != 32 || rr.Header().Get(RequestIDHeader) != seen {
		t.Errorf("generated id %q, header %q", seen, rr.Header().Get(RequestIDHeader))
	}

	// Accepted when well-formed.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "client-abc.123")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != "client-abc.123" {
		t.Errorf("inbound id not propagated: %q", seen)
	}

	// Replaced when malformed (log-injection guard).
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "bad id\nwith newline")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if strings.ContainsAny(seen, " \n") || len(seen) != 32 {
		t.Errorf("malformed id not replaced: %q", seen)
	}
}

func TestRecoverReturnsProblem(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), RequestID(), Recover(log))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("content-type = %q", ct)
	}
	var p Problem
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 500 || p.RequestID == "" || p.Instance != "/x" {
		t.Errorf("problem = %+v", p)
	}
	if strings.Contains(rr.Body.String(), "boom") {
		t.Error("panic value leaked to client")
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Error("panic value not logged")
	}
}

func TestNotFoundIsProblemJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	NotFound().ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nope", nil))
	if rr.Code != 404 || rr.Header().Get("Content-Type") != ProblemContentType {
		t.Errorf("got %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
}

func TestAccessLogDoesNotLogQueryOrHeaders(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), RequestID(), AccessLog(log))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/p?token=s3cret", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := logs.String()
	if strings.Contains(out, "s3cret") {
		t.Errorf("access log leaked secret: %s", out)
	}
	if !strings.Contains(out, `"status":418`) || !strings.Contains(out, `"path":"/p"`) {
		t.Errorf("access log missing fields: %s", out)
	}
}
