// Package httpx holds HTTP plumbing shared by every handler: RFC 9457
// problem responses, request IDs, panic recovery and access logging.
package httpx

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is the RFC 9457 media type.
const ProblemContentType = "application/problem+json"

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteProblem writes p with the problem content type. The request ID is
// filled from the request context when present.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if r != nil {
		if p.RequestID == "" {
			p.RequestID = RequestIDFrom(r.Context())
		}
		if p.Instance == "" && r.URL != nil {
			p.Instance = r.URL.Path
		}
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// NotFound is the fallback handler for unrouted paths.
func NotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, Problem{Status: http.StatusNotFound})
	})
}
