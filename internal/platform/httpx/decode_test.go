package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	cases := []struct {
		in   string
		ok   bool
		code int
	}{
		{`{"name":"x"}`, true, 0},
		{`{"name":"x","extra":1}`, false, http.StatusBadRequest},
		{`{"name":"x"}{"name":"y"}`, false, http.StatusBadRequest},
		{`not json`, false, http.StatusBadRequest},
		{`{"name":"` + strings.Repeat("a", MaxJSONBody) + `"}`, false, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tc.in))
		var b body
		ok := DecodeJSON(rr, req, &b)
		if ok != tc.ok || (!ok && rr.Code != tc.code) {
			t.Errorf("%.40q: ok=%v code=%d, want ok=%v code=%d", tc.in, ok, rr.Code, tc.ok, tc.code)
		}
		if !ok && rr.Header().Get("Content-Type") != ProblemContentType {
			t.Errorf("%.40q: not a problem response", tc.in)
		}
	}
}
