package nodes

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
	svc *Service
}

func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// MountAuthed mounts routes that require a session cookie.
func (h *Handlers) MountAuthed(r chi.Router) {
	r.Post("/v1/nodes/pair-token", h.createPairToken)
}

// MountPublic mounts routes accessible without a session (token-authenticated).
func (h *Handlers) MountPublic(r chi.Router) {
	r.Post("/v1/nodes/pair", h.pair)
}

type pairTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *Handlers) createPairToken(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	token, err := h.svc.CreatePairToken(r.Context(), uid)
	if err != nil {
		slog.Error("create pair token", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pairTokenResp{
		Token:     token,
		ExpiresAt: time.Now().Add(pairTokenTTL),
	})
}

type pairReq struct {
	Token   string          `json:"token"`
	Name    string          `json:"name"`
	GPUInfo json.RawMessage `json:"gpu_info"`
}

type pairResp struct {
	NodeID    string `json:"node_id"`
	NodeToken string `json:"node_token"`
}

func (h *Handlers) pair(w http.ResponseWriter, r *http.Request) {
	var req pairReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Token == "" || req.Name == "" {
		httperr.Write(w, http.StatusBadRequest, "token and name required")
		return
	}
	node, nodeToken, err := h.svc.PairNode(r.Context(), PairRequest{
		Token:   req.Token,
		Name:    req.Name,
		GPUInfo: []byte(req.GPUInfo),
	})
	switch {
	case errors.Is(err, ErrTokenInvalid):
		httperr.Write(w, http.StatusUnauthorized, "invalid pair token")
		return
	case errors.Is(err, ErrNodeNameTaken):
		httperr.Write(w, http.StatusConflict, "node name taken")
		return
	case err != nil:
		slog.Error("pair node", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pairResp{
		NodeID:    node.ID.String(),
		NodeToken: nodeToken,
	})
}
