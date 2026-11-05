package auth

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/httpx"
)

type ctxKey int

const principalKey ctxKey = iota

// PrincipalFrom returns the authenticated caller stored by RequireAuth.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// Handler serves the authentication endpoints (plan 15: Auth).
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler returns HTTP handlers for svc.
func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register mounts the endpoints on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.Handle("POST /v1/auth/logout", h.RequireAuth(http.HandlerFunc(h.logout)))
	mux.Handle("GET /v1/me", h.RequireAuth(http.HandlerFunc(h.me)))
}

// unauthorized writes a 401 with a Bearer challenge. Unauthenticated requests
// to protected endpoints are not audited (plan 7.1).
func unauthorized(w http.ResponseWriter, r *http.Request, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="dataset-studio"`)
	httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Detail: detail})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// RequireAuth authenticates the Bearer credential and stores the Principal in
// the request context, or answers 401.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok {
			unauthorized(w, r, "authentication required")
			return
		}
		p, err := h.svc.Authenticate(r.Context(), tok)
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				h.log.ErrorContext(r.Context(), "authenticate", slog.String("request_id", httpx.RequestIDFrom(r.Context())),
					slog.String("error", err.Error()))
				httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusInternalServerError})
				return
			}
			unauthorized(w, r, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token       string `json:"token"`
	TokenType   string `json:"token_type"`
	ExpiresAt   string `json:"expires_at"`
	PrincipalID string `json:"principal_id"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	res, err := h.svc.Login(r.Context(), LoginInput{
		Email: req.Email, Password: req.Password, IP: clientIP(r),
		UserAgent: r.UserAgent(), RequestID: httpx.RequestIDFrom(r.Context()),
	})
	var limited *RateLimitedError
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, loginResponse{
			Token: res.Token, TokenType: "Bearer", ExpiresAt: res.ExpiresAt.Format(time.RFC3339),
			PrincipalID: res.PrincipalID.String(),
		})
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(limited.RetryAfter.Seconds()))))
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusTooManyRequests, Detail: "too many login attempts"})
	case errors.Is(err, ErrInvalidCredentials):
		// One body for every credential failure (plan 14).
		unauthorized(w, r, "invalid credentials")
	default:
		h.log.ErrorContext(r.Context(), "login", slog.String("request_id", httpx.RequestIDFrom(r.Context())),
			slog.String("error", err.Error()))
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusInternalServerError})
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	err := h.svc.Logout(r.Context(), p, httpx.RequestIDFrom(r.Context()))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNotSession):
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusBadRequest,
			Detail: "logout ends a session; API keys are revoked instead"})
	case errors.Is(err, ErrUnauthenticated):
		unauthorized(w, r, "authentication required")
	default:
		h.log.ErrorContext(r.Context(), "logout", slog.String("request_id", httpx.RequestIDFrom(r.Context())),
			slog.String("error", err.Error()))
		httpx.WriteProblem(w, r, httpx.Problem{Status: http.StatusInternalServerError})
	}
}

type meResponse struct {
	PrincipalID string `json:"principal_id"`
	Kind        string `json:"kind"`
	AuthMethod  string `json:"auth_method"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, meResponse{PrincipalID: p.ID.String(), Kind: string(p.Kind), AuthMethod: string(p.Method)})
}
