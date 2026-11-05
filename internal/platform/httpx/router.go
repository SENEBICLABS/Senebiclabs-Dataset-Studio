package httpx

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Route is one API endpoint. Path is written as the plan documents it (plan
// section 15), including custom-method suffixes such as
// "/v1/users/{id}:disable".
type Route struct {
	Method  string
	Path    string
	Handler http.Handler
}

// String is "METHOD /path", the form the route tests key on.
func (r Route) String() string { return r.Method + " " + r.Path }

// splitAction separates a trailing ":action" from the last segment of a
// documented path. "/v1/users/{id}:disable" gives ("/v1/users/{id}", "id",
// "disable") and "/v1/users/{id}" gives ("/v1/users/{id}", "id", ""). A path
// whose last segment is not a wildcard has no wildcard and no action.
func splitAction(path string) (base, wildcard, action string) {
	i := strings.LastIndex(path, "/")
	last := path[i+1:]
	if !strings.HasPrefix(last, "{") {
		return path, "", ""
	}
	end := strings.Index(last, "}")
	switch {
	case end < 0 || strings.HasSuffix(last[:end], "..."):
		return path, "", ""
	case end == len(last)-1:
		return path, last[1:end], "" // a plain "{id}" segment
	case last[end+1] != ':':
		return path, "", ""
	}
	return path[:i+1] + last[:end+1], last[1:end], last[end+2:]
}

// Mount registers routes on mux. net/http patterns cannot match part of a
// segment, so routes that share a method and a base path ending in a wildcard
// ("{id}" and "{id}:disable") are registered once, and a dispatcher splits the
// segment at the first ':' and routes on the action. The wildcard's path
// value is the part before the ':'. An unknown action is 404.
func Mount(mux *http.ServeMux, routes []Route) error {
	type group struct {
		pattern  string
		wildcard string
		actions  map[string]http.Handler // "" is the plain path
	}
	groups := map[string]*group{}
	for _, rt := range routes {
		base, wildcard, action := splitAction(rt.Path)
		// Group by the base path with the last wildcard's name erased, so
		// "{id}:a" and "{uid}:b" land in one group and conflict here.
		key := rt.Method + " " + base
		if wildcard != "" {
			key = rt.Method + " " + strings.TrimSuffix(base, "{"+wildcard+"}") + "{}"
		}
		g, ok := groups[key]
		if !ok {
			g = &group{pattern: rt.Method + " " + base, actions: map[string]http.Handler{}}
			groups[key] = g
		}
		if wildcard != "" {
			if g.wildcard != "" && g.wildcard != wildcard {
				return fmt.Errorf("httpx: %s: wildcard {%s} conflicts with {%s}", rt, wildcard, g.wildcard)
			}
			g.wildcard = wildcard
		}
		if _, dup := g.actions[action]; dup {
			return fmt.Errorf("httpx: duplicate route %s", rt)
		}
		g.actions[action] = rt.Handler
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		h := g.actions[""]
		if g.wildcard != "" {
			h = dispatch(g.wildcard, g.actions)
		}
		if err := handle(mux, g.pattern, h); err != nil {
			return err
		}
	}
	return nil
}

// handle registers one pattern, turning the mux's panic on an invalid or
// conflicting pattern into an error.
func handle(mux *http.ServeMux, pattern string, h http.Handler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("httpx: register %s: %v", pattern, p)
		}
	}()
	mux.Handle(pattern, h)
	return nil
}

func dispatch(wildcard string, actions map[string]http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, action, _ := strings.Cut(r.PathValue(wildcard), ":")
		h, ok := actions[action]
		if !ok {
			WriteProblem(w, r, Problem{Status: http.StatusNotFound})
			return
		}
		r.SetPathValue(wildcard, value)
		h.ServeHTTP(w, r)
	})
}
