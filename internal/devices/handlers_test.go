//go:build integration

package devices_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/devices"
)

func mountWithUID(t *testing.T, svc *devices.Service, uid uuid.UUID) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.WithUserID(req.Context(), uid)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	devices.NewHandlers(svc).Mount(r)
	return r
}

func TestPair_Returns201WithKey(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)
	_, err := hs.CreateUser(ctx, "paul")
	require.NoError(t, err)
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret123")
	require.NoError(t, err)
	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	h := mountWithUID(t, svc, u.ID)

	body, _ := json.Marshal(map[string]string{"name": "macbook"})
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/pair", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp struct {
		DeviceID      string `json:"device_id"`
		Hostname      string `json:"hostname"`
		PreauthKey    string `json:"preauth_key"`
		HeadscaleURL  string `json:"headscale_url"`
		TailnetDomain string `json:"tailnet_domain"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "paul-device-macbook", resp.Hostname)
	require.NotEmpty(t, resp.PreauthKey)
	require.Equal(t, "https://hs.example.com", resp.HeadscaleURL)
	require.Equal(t, "flex", resp.TailnetDomain)
}

func TestPair_InvalidNameReturns400(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)
	_, _ = hs.CreateUser(ctx, "paul")
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret123")
	require.NoError(t, err)
	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	h := mountWithUID(t, svc, u.ID)

	body, _ := json.Marshal(map[string]string{"name": "BAD NAME"})
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/pair", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestList_ReturnsOnlyOwnDevices(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)
	_, _ = hs.CreateUser(ctx, "alice")
	_, _ = hs.CreateUser(ctx, "bob")
	u1, err := usersSvc.Signup(ctx, "a@x.com", "alice", "supersecret123")
	require.NoError(t, err)
	u2, err := usersSvc.Signup(ctx, "b@x.com", "bob", "supersecret123")
	require.NoError(t, err)
	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	_, err = svc.Pair(ctx, u1.ID, "laptop")
	require.NoError(t, err)
	_, err = svc.Pair(ctx, u2.ID, "laptop")
	require.NoError(t, err)

	h := mountWithUID(t, svc, u1.ID)
	req := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "alice-device-laptop", out[0]["hostname"])
}

func TestDelete_NotFoundOnCrossUser(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)
	_, _ = hs.CreateUser(ctx, "alice")
	_, _ = hs.CreateUser(ctx, "bob")
	u1, err := usersSvc.Signup(ctx, "a@x.com", "alice", "supersecret123")
	require.NoError(t, err)
	u2, err := usersSvc.Signup(ctx, "b@x.com", "bob", "supersecret123")
	require.NoError(t, err)
	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	res, err := svc.Pair(ctx, u1.ID, "laptop")
	require.NoError(t, err)

	h := mountWithUID(t, svc, u2.ID)
	req := httptest.NewRequest(http.MethodDelete, "/v1/devices/"+res.Device.ID.String(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
