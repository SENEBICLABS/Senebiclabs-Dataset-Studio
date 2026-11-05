package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func echo(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reported in a header, not the body: the test only compares values.
		w.Header().Set("X-Route", name+":"+r.PathValue("id"))
	})
}

func TestMountRoutesCustomMethods(t *testing.T) {
	mux := http.NewServeMux()
	if err := Mount(mux, []Route{
		{Method: http.MethodGet, Path: "/v1/users/{id}", Handler: echo("get")},
		{Method: http.MethodPost, Path: "/v1/users/{id}:disable", Handler: echo("disable")},
		{Method: http.MethodPost, Path: "/v1/things/{id}", Handler: echo("plain")},
		{Method: http.MethodPost, Path: "/v1/things/{id}:archive", Handler: echo("archive")},
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path string
		code         int
		body         string
	}{
		{http.MethodGet, "/v1/users/abc", 200, "get:abc"},
		{http.MethodPost, "/v1/users/abc:disable", 200, "disable:abc"},
		{http.MethodPost, "/v1/users/abc", 404, ""},
		{http.MethodPost, "/v1/users/abc:enable", 404, ""},
		{http.MethodPost, "/v1/things/t1", 200, "plain:t1"},
		{http.MethodPost, "/v1/things/t1:archive", 200, "archive:t1"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), c.method, c.path, nil))
		if got := rr.Header().Get("X-Route"); rr.Code != c.code || got != c.body {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.path, rr.Code, got, c.code, c.body)
		}
	}
}

func TestMountRejectsDuplicates(t *testing.T) {
	r := Route{Method: http.MethodPost, Path: "/v1/users/{id}:disable", Handler: echo("x")}
	if err := Mount(http.NewServeMux(), []Route{r, r}); err == nil {
		t.Error("duplicate route accepted")
	}
	if err := Mount(http.NewServeMux(), []Route{
		{Method: http.MethodPost, Path: "/v1/users/{id}:a", Handler: echo("a")},
		{Method: http.MethodPost, Path: "/v1/users/{uid}:b", Handler: echo("b")},
	}); err == nil {
		t.Error("conflicting wildcard names accepted")
	}
}
