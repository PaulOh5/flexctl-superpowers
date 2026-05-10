package users

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/audit"
	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

// Policy is a hook fired after a user is successfully signed up.
// Failure indicates the post-commit synchronization failed; the signup
// handler will compensate by deleting the user row.
type Policy interface {
	OnUserCreated(ctx context.Context, slug string) error
}

// NoOpPolicy is a Policy that does nothing — useful for tests and for the
// MVP boot path where Headscale has not been wired yet.
type NoOpPolicy struct{}

func (NoOpPolicy) OnUserCreated(_ context.Context, _ string) error { return nil }

type Handlers struct {
	svc    *Service
	signer *auth.SessionSigner
	pool   *pgxpool.Pool
	policy Policy
}

func NewHandlers(svc *Service, signer *auth.SessionSigner, pool *pgxpool.Pool, policy Policy) *Handlers {
	if policy == nil {
		policy = NoOpPolicy{}
	}
	return &Handlers{svc: svc, signer: signer, pool: pool, policy: policy}
}

func (h *Handlers) Mount(r chi.Router) {
	r.Post("/v1/auth/signup", h.signup)
	r.Post("/v1/auth/login", h.login)
}

func (h *Handlers) MountAuthed(r chi.Router) {
	r.Get("/v1/me", h.me)
	r.Post("/v1/auth/logout", h.logout)
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	u, err := h.svc.ByID(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		httperr.Write(w, http.StatusUnauthorized, "user gone")
		return
	}
	if err != nil {
		slog.Error("me lookup", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}

type signupReq struct {
	Email    string `json:"email"`
	Slug     string `json:"slug"`
	Password string `json:"password"`
}

type meResp struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Slug  string `json:"slug"`
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := h.svc.Authenticate(r.Context(), req.Email, req.Password)
	if errors.Is(err, ErrBadCredentials) {
		_ = audit.Log(r.Context(), h.pool, audit.Event{
			Action: "auth.login_failed",
			Target: req.Email,
			IP:     net.ParseIP(remoteIP(r)),
		})
		httperr.Write(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		slog.Error("login internal", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}

	_ = audit.Log(r.Context(), h.pool, audit.Event{
		UserID: &u.ID,
		Action: "auth.login",
		Target: u.Slug,
		IP:     net.ParseIP(remoteIP(r)),
	})
	if !h.issueSession(w, r, u) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}

func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) {
	if uid, ok := auth.UserIDFrom(r.Context()); ok {
		_ = audit.Log(r.Context(), h.pool, audit.Event{
			UserID: &uid,
			Action: "auth.logout",
			IP:     net.ParseIP(remoteIP(r)),
		})
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) signup(w http.ResponseWriter, r *http.Request) {
	var req signupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := h.svc.Signup(r.Context(), req.Email, req.Slug, req.Password)
	switch {
	case errors.Is(err, ErrEmailTaken):
		httperr.Write(w, http.StatusConflict, "email taken")
		return
	case errors.Is(err, ErrSlugTaken):
		httperr.Write(w, http.StatusConflict, "slug taken")
		return
	case errors.Is(err, ErrInvalidEmail):
		httperr.Write(w, http.StatusBadRequest, "invalid email")
		return
	case errors.Is(err, ErrInvalidSlug):
		httperr.Write(w, http.StatusBadRequest, "invalid slug")
		return
	case errors.Is(err, ErrPasswordTooShort):
		httperr.Write(w, http.StatusBadRequest, "password too short")
		return
	case err != nil:
		slog.Error("signup internal", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}

	_ = audit.Log(r.Context(), h.pool, audit.Event{
		UserID:   &u.ID,
		Action:   "user.signup",
		Target:   u.Slug,
		Metadata: map[string]any{"email": u.Email},
		IP:       net.ParseIP(remoteIP(r)),
	})
	if err := h.policy.OnUserCreated(r.Context(), u.Slug); err != nil {
		slog.Error("policy on-user-created", "err", err, "slug", u.Slug)
		if delErr := h.svc.HardDelete(r.Context(), u.ID); delErr != nil {
			slog.Error("rollback hard-delete", "err", delErr, "user_id", u.ID)
		}
		httperr.Write(w, http.StatusInternalServerError, "registration failed")
		return
	}
	if !h.issueSession(w, r, u) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}

// issueSession encodes a 30-day session for u and writes the flex_session cookie.
// Returns false (and writes a 500 response) on encode failure; caller should return.
func (h *Handlers) issueSession(w http.ResponseWriter, r *http.Request, u User) bool {
	expires := time.Now().Add(30 * 24 * time.Hour)
	token, err := h.signer.Encode(auth.Session{
		UserID:    u.ID,
		ExpiresAt: expires,
	})
	if err != nil {
		slog.Error("session encode", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "session encode")
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	return true
}

// remoteIP returns the client IP from r.RemoteAddr.
// chi/middleware.RealIP has already replaced RemoteAddr with the real client IP
// extracted from X-Forwarded-For / X-Real-IP, so we just split off the port.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
