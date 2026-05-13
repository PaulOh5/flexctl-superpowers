package devices

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

// Handlers wires HTTP routes to Service.
type Handlers struct{ svc *Service }

// NewHandlers constructs Handlers from svc.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Mount registers the devices routes on r.
func (h *Handlers) Mount(r chi.Router) {
	r.Post("/v1/devices/pair", h.pair)
	r.Get("/v1/devices", h.list)
	r.Delete("/v1/devices/{id}", h.delete)
}

type pairReq struct {
	Name string `json:"name"`
}

type pairResp struct {
	DeviceID      string `json:"device_id"`
	Hostname      string `json:"hostname"`
	PreauthKey    string `json:"preauth_key"`
	HeadscaleURL  string `json:"headscale_url"`
	TailnetDomain string `json:"tailnet_domain"`
}

type deviceResp struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hostname   string     `json:"hostname"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

func (h *Handlers) pair(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var req pairReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	res, err := h.svc.Pair(r.Context(), uid, req.Name)
	switch {
	case errors.Is(err, ErrInvalidName):
		httperr.Write(w, http.StatusBadRequest, "invalid device name")
		return
	case errors.Is(err, ErrConflict):
		httperr.Write(w, http.StatusConflict, "device hostname already taken")
		return
	case err != nil:
		slog.Error("devices.Pair", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pairResp{
		DeviceID:      res.Device.ID.String(),
		Hostname:      res.Device.Hostname,
		PreauthKey:    res.PreauthKey,
		HeadscaleURL:  res.HeadscaleURL,
		TailnetDomain: res.TailnetDomain,
	})
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	got, err := h.svc.List(r.Context(), uid)
	if err != nil {
		slog.Error("devices.List", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]deviceResp, 0, len(got))
	for _, d := range got {
		out = append(out, deviceResp{
			ID: d.ID.String(), Name: d.Name, Hostname: d.Hostname,
			CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt,
		})
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
	switch {
	case errors.Is(err, ErrNotFound):
		httperr.Write(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		slog.Error("devices.Delete", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
