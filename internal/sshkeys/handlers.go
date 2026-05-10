package sshkeys

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

type Handlers struct {
	svc *Service
}

func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

func (h *Handlers) Mount(r chi.Router) {
	r.Get("/v1/me/ssh-keys", h.list)
	r.Post("/v1/me/ssh-keys", h.add)
	r.Delete("/v1/me/ssh-keys/{id}", h.delete)
}

type addReq struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

type keyResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

func (h *Handlers) add(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var req addReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Name == "" {
		httperr.Write(w, http.StatusBadRequest, "name required")
		return
	}
	k, err := h.svc.Add(r.Context(), uid, req.Name, req.PublicKey)
	switch {
	case errors.Is(err, ErrInvalidKey):
		httperr.Write(w, http.StatusBadRequest, "invalid public key")
		return
	case errors.Is(err, ErrDuplicate):
		httperr.Write(w, http.StatusConflict, "duplicate key")
		return
	case err != nil:
		slog.Error("ssh-keys add", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(keyResp{ID: k.ID.String(), Name: k.Name, PublicKey: k.PublicKey, Fingerprint: k.Fingerprint})
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	keys, err := h.svc.List(r.Context(), uid)
	if err != nil {
		slog.Error("ssh-keys list", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]keyResp, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyResp{ID: k.ID.String(), Name: k.Name, PublicKey: k.PublicKey, Fingerprint: k.Fingerprint})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id")
		return
	}
	err = h.svc.Delete(r.Context(), uid, id)
	if errors.Is(err, ErrNotFound) {
		httperr.Write(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		slog.Error("ssh-keys delete", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
