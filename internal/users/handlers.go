package users

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

type Handlers struct {
	svc    *Service
	signer *auth.SessionSigner
}

func NewHandlers(svc *Service, signer *auth.SessionSigner) *Handlers {
	return &Handlers{svc: svc, signer: signer}
}

func (h *Handlers) Mount(r chi.Router) {
	r.Post("/v1/auth/signup", h.signup)
	r.Post("/v1/auth/login", h.login)
}

func (h *Handlers) MountAuthed(r chi.Router) {
	r.Get("/v1/me", h.me)
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
		httperr.Write(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		slog.Error("login internal", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}

	token, err := h.signer.Encode(auth.Session{
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		slog.Error("login session encode", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "session encode")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "flex_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
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

	token, err := h.signer.Encode(auth.Session{
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		slog.Error("signup session encode", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "session encode")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "flex_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}
