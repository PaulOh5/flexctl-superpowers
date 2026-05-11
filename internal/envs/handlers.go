package envs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

// Dispatcher dispatches env lifecycle commands to the relevant agent. The
// production impl uses the agent gRPC stream; tests pass a fake.
type Dispatcher interface {
	Create(ctx context.Context, envID uuid.UUID) error
	Stop(ctx context.Context, envID uuid.UUID) error
	Start(ctx context.Context, envID uuid.UUID) error
	Delete(ctx context.Context, envID uuid.UUID) error
}

type Handlers struct {
	svc  *Service
	disp Dispatcher
}

func NewHandlers(svc *Service, disp Dispatcher) *Handlers {
	return &Handlers{svc: svc, disp: disp}
}

func (h *Handlers) Mount(r chi.Router) {
	r.Get("/v1/envs", h.list)
	r.Post("/v1/envs", h.create)
	r.Get("/v1/envs/{id}", h.get)
	r.Post("/v1/envs/{id}/stop", h.stop)
	r.Post("/v1/envs/{id}/start", h.start)
	r.Delete("/v1/envs/{id}", h.delete)
}

type createReq struct {
	NodeID     string `json:"node_id"`
	TemplateID string `json:"template_id"`
	Name       string `json:"name"`
	GPURequest int32  `json:"gpu_request"`
}

type envResp struct {
	ID                 string  `json:"id"`
	NodeID             string  `json:"node_id"`
	TemplateID         string  `json:"template_id"`
	Name               string  `json:"name"`
	Hostname           string  `json:"hostname"`
	Status             string  `json:"status"`
	StatusMessage      string  `json:"status_message,omitempty"`
	SidecarContainerID string  `json:"sidecar_container_id,omitempty"`
	DevContainerID     string  `json:"dev_container_id,omitempty"`
	GPURequest         int32   `json:"gpu_request"`
	GPUIndices         []int32 `json:"gpu_indices"`
}

func toResp(e Env) envResp {
	return envResp{
		ID: e.ID.String(), NodeID: e.NodeID.String(), TemplateID: e.TemplateID,
		Name: e.Name, Hostname: e.Hostname, Status: e.Status,
		StatusMessage: e.StatusMessage, SidecarContainerID: e.SidecarContainerID,
		DevContainerID: e.DevContainerID, GPURequest: e.GPURequest,
		GPUIndices: e.GPUIndices,
	}
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json"); return
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid node_id"); return
	}
	if req.GPURequest < 0 {
		httperr.Write(w, http.StatusBadRequest, "gpu_request must be >= 0"); return
	}
	env, err := h.svc.Create(r.Context(), CreateRequest{
		OwnerUserID: uid, NodeID: nodeID, TemplateID: req.TemplateID,
		Name: req.Name, GPURequest: req.GPURequest,
	})
	switch {
	case errors.Is(err, ErrInvalidName):
		httperr.Write(w, http.StatusBadRequest, "invalid env name"); return
	case errors.Is(err, ErrNameTaken):
		httperr.Write(w, http.StatusConflict, "env name taken"); return
	case err != nil:
		slog.Error("envs.Create", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}

	// Dispatch. On failure roll back DB row.
	if err := h.disp.Create(r.Context(), env.ID); err != nil {
		slog.Error("dispatch create", "err", err, "env_id", env.ID)
		if delErr := h.svc.Delete(r.Context(), env.ID); delErr != nil {
			slog.Error("rollback delete", "err", delErr, "env_id", env.ID)
		}
		httperr.Write(w, http.StatusInternalServerError, "dispatch failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(toResp(env))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	got, err := h.svc.ListByOwner(r.Context(), uid)
	if err != nil {
		slog.Error("envs.ListByOwner", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	out := make([]envResp, 0, len(got))
	for _, e := range got {
		out = append(out, toResp(e))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id"); return
	}
	env, err := h.svc.ByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && env.OwnerUserID != uid) {
		httperr.Write(w, http.StatusNotFound, "not found"); return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toResp(env))
}

func (h *Handlers) stop(w http.ResponseWriter, r *http.Request)   { h.action(w, r, h.disp.Stop) }
func (h *Handlers) start(w http.ResponseWriter, r *http.Request)  { h.action(w, r, h.disp.Start) }
func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) { h.action(w, r, h.disp.Delete) }

func (h *Handlers) action(w http.ResponseWriter, r *http.Request, fn func(context.Context, uuid.UUID) error) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id"); return
	}
	env, err := h.svc.ByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && env.OwnerUserID != uid) {
		httperr.Write(w, http.StatusNotFound, "not found"); return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	if err := fn(r.Context(), id); err != nil {
		slog.Error("dispatch", "err", err, "env_id", id)
		httperr.Write(w, http.StatusInternalServerError, "dispatch failed"); return
	}
	w.WriteHeader(http.StatusAccepted)
}
