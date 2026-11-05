package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/authz"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/httpx"
)

// Handler serves the administration endpoints. It decodes and encodes only;
// authorization, transactions and audit are in Service.
type Handler struct {
	svc         *Service
	log         *slog.Logger
	requireAuth func(http.Handler) http.Handler
}

// NewHandler returns HTTP handlers for svc. requireAuth authenticates the
// caller (auth.Handler.RequireAuth).
func NewHandler(svc *Service, log *slog.Logger, requireAuth func(http.Handler) http.Handler) *Handler {
	return &Handler{svc: svc, log: log, requireAuth: requireAuth}
}

// Routes lists the endpoints (plan 15: Users; Service accounts and keys).
//
// The plan writes the key endpoints as "POST/GET /v1/{principal}/api-keys".
// A bare principal segment directly under /v1 would collide with
// /v1/users/{id}, so they are served under /v1/principals/{id}/api-keys.
func (h *Handler) Routes() []httpx.Route {
	r := func(method, path string, fn http.HandlerFunc) httpx.Route {
		return httpx.Route{Method: method, Path: path, Handler: h.requireAuth(fn)}
	}
	return []httpx.Route{
		r(http.MethodGet, "/v1/users", h.listUsers),
		r(http.MethodPost, "/v1/users", h.createUser),
		r(http.MethodGet, "/v1/users/{id}", h.getUser),
		r(http.MethodPatch, "/v1/users/{id}", h.updateUser),
		r(http.MethodPost, "/v1/users/{id}:disable", h.disableUser),
		r(http.MethodGet, "/v1/service-accounts", h.listServiceAccounts),
		r(http.MethodPost, "/v1/service-accounts", h.createServiceAccount),
		r(http.MethodPost, "/v1/principals/{id}/api-keys", h.issueAPIKey),
		r(http.MethodGet, "/v1/principals/{id}/api-keys", h.listAPIKeys),
		r(http.MethodDelete, "/v1/api-keys/{id}", h.revokeAPIKey),
	}
}

// fail maps service errors to problem responses. 403 bodies are uniform.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, authz.ErrDenied):
		if errors.Is(err, authz.ErrDenialNotRecorded) {
			// Still a 403, but the audit gap must be visible.
			h.log.ErrorContext(r.Context(), "denied event not recorded", slog.String("op", op),
				slog.String("request_id", httpx.RequestIDFrom(r.Context())), slog.String("error", err.Error()))
		}
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusForbidden, Detail: "permission denied"})
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusNotFound})
	case errors.Is(err, ErrConflict):
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusConflict, Detail: err.Error()})
	case errors.Is(err, auth.ErrInvalidInput):
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
	default:
		h.log.ErrorContext(r.Context(), op, slog.String("request_id", httpx.RequestIDFrom(r.Context())),
			slog.String("error", err.Error()))
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusInternalServerError})
	}
}

func caller(r *http.Request) (auth.Principal, string) {
	p, _ := auth.PrincipalFrom(r.Context())
	return p, httpx.RequestIDFrom(r.Context())
}

// pathID parses the {id} wildcard. A malformed id becomes uuid.Nil, which
// names no principal or key: the service still authorizes first (403 and a
// denied event for a refused caller) and then finds nothing (404).
func pathID(r *http.Request) uuid.UUID {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil
	}
	return id
}

func page(r *http.Request) (Page, error) {
	q := r.URL.Query()
	p := Page{Cursor: q.Get("cursor")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Page{}, invalid("limit must be an integer between 1 and %d", MaxPageSize)
		}
		p.Limit = n
	}
	return p, nil
}

type userJSON struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	GlobalRole  string `json:"global_role"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

func toUserJSON(u User) userJSON {
	return userJSON{ID: u.ID.String(), Email: u.Email, DisplayName: u.DisplayName, GlobalRole: u.GlobalRole,
		Status: u.Status, CreatedAt: u.CreatedAt.Format(time.RFC3339Nano)}
}

type listJSON[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	p, rid := caller(r)
	pg, err := page(r)
	if err == nil {
		var users []User
		var next string
		if users, next, err = h.svc.ListUsers(r.Context(), p, pg, rid); err == nil {
			out := listJSON[userJSON]{Items: make([]userJSON, len(users)), NextCursor: next}
			for i, u := range users {
				out.Items[i] = toUserJSON(u)
			}
			httpx.WriteJSON(w, http.StatusOK, out)
			return
		}
	}
	h.fail(w, r, "list users", err)
}

type createUserRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	GlobalRole  string `json:"global_role"`
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	p, rid := caller(r)
	u, err := h.svc.CreateUser(r.Context(), p, CreateUserInput(req), rid)
	if err != nil {
		h.fail(w, r, "create user", err)
		return
	}
	w.Header().Set("Location", "/v1/users/"+u.ID.String())
	httpx.WriteJSON(w, http.StatusCreated, toUserJSON(u))
}

func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	p, rid := caller(r)
	u, err := h.svc.GetUser(r.Context(), p, pathID(r), rid)
	if err != nil {
		h.fail(w, r, "get user", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUserJSON(u))
}

type updateUserRequest struct {
	DisplayName *string `json:"display_name"`
	GlobalRole  *string `json:"global_role"`
	Password    *string `json:"password"`
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	var req updateUserRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	p, rid := caller(r)
	u, err := h.svc.UpdateUser(r.Context(), p, pathID(r), UpdateUserInput(req), rid)
	if err != nil {
		h.fail(w, r, "update user", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUserJSON(u))
}

func (h *Handler) disableUser(w http.ResponseWriter, r *http.Request) {
	p, rid := caller(r)
	u, err := h.svc.DisableUser(r.Context(), p, pathID(r), rid)
	if err != nil {
		h.fail(w, r, "disable user", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUserJSON(u))
}

type serviceAccountJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description"`
	GlobalRole  string `json:"global_role"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

func toServiceAccountJSON(sa ServiceAccount) serviceAccountJSON {
	return serviceAccountJSON{ID: sa.ID.String(), Name: sa.Name, Description: sa.Description,
		GlobalRole: sa.GlobalRole, Status: sa.Status, CreatedAt: sa.CreatedAt.Format(time.RFC3339Nano)}
}

func (h *Handler) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	p, rid := caller(r)
	pg, err := page(r)
	if err == nil {
		var sas []ServiceAccount
		var next string
		if sas, next, err = h.svc.ListServiceAccounts(r.Context(), p, pg, rid); err == nil {
			out := listJSON[serviceAccountJSON]{Items: make([]serviceAccountJSON, len(sas)), NextCursor: next}
			for i, sa := range sas {
				out.Items[i] = toServiceAccountJSON(sa)
			}
			httpx.WriteJSON(w, http.StatusOK, out)
			return
		}
	}
	h.fail(w, r, "list service accounts", err)
}

type createServiceAccountRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	GlobalRole  string `json:"global_role"`
}

func (h *Handler) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	var req createServiceAccountRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	p, rid := caller(r)
	sa, err := h.svc.CreateServiceAccount(r.Context(), p, CreateServiceAccountInput(req), rid)
	if err != nil {
		h.fail(w, r, "create service account", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toServiceAccountJSON(sa))
}

type issueAPIKeyRequest struct {
	ExpiresAt string `json:"expires_at"`
}

type issuedAPIKeyJSON struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Prefix    string `json:"prefix"`
	ExpiresAt string `json:"expires_at"`
}

func (h *Handler) issueAPIKey(w http.ResponseWriter, r *http.Request) {
	var req issueAPIKeyRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	p, rid := caller(r)
	// A malformed time becomes the zero time, which the service rejects
	// after authorizing, so a refused caller is always 403 and audited.
	exp, _ := time.Parse(time.RFC3339, req.ExpiresAt)
	k, err := h.svc.IssueAPIKey(r.Context(), p, pathID(r), exp, rid)
	if err != nil {
		h.fail(w, r, "issue api key", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, issuedAPIKeyJSON{
		ID: k.ID.String(), Key: k.Key, Prefix: k.Prefix, ExpiresAt: k.ExpiresAt.Format(time.RFC3339),
	})
}

type apiKeyJSON struct {
	ID        string  `json:"id"`
	Prefix    string  `json:"prefix"`
	CreatedAt string  `json:"created_at"`
	CreatedBy string  `json:"created_by"`
	ExpiresAt string  `json:"expires_at"`
	RevokedAt *string `json:"revoked_at"`
}

func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	p, rid := caller(r)
	keys, err := h.svc.ListAPIKeys(r.Context(), p, pathID(r), rid)
	if err != nil {
		h.fail(w, r, "list api keys", err)
		return
	}
	out := listJSON[apiKeyJSON]{Items: make([]apiKeyJSON, len(keys))}
	for i, k := range keys {
		j := apiKeyJSON{ID: k.ID.String(), Prefix: k.Prefix, CreatedAt: k.CreatedAt.Format(time.RFC3339Nano),
			CreatedBy: k.CreatedBy.String(), ExpiresAt: k.ExpiresAt.Format(time.RFC3339)}
		if k.RevokedAt != nil {
			v := k.RevokedAt.Format(time.RFC3339Nano)
			j.RevokedAt = &v
		}
		out.Items[i] = j
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	p, rid := caller(r)
	if err := h.svc.RevokeAPIKey(r.Context(), p, pathID(r), rid); err != nil {
		h.fail(w, r, "revoke api key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
