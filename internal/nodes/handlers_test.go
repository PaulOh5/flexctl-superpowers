package nodes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/nodes"
)

func mountForTest(t *testing.T, _ *pgxpool.Pool, signer *auth.SessionSigner, h *nodes.Handlers) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	h.MountPublic(r)
	return httptest.NewServer(r)
}

func TestPairTokenHandler_RequiresAuth(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/nodes/pair-token", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPairTokenHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	uid := mkUser(t, pool, "p@x.com", "paul")
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/nodes/pair-token", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.Token)
	require.NotEmpty(t, got.ExpiresAt)
}

func TestPairHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	uid := mkUser(t, pool, "p@x.com", "paul")
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	pairToken, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]any{
		"token": pairToken,
		"name":  "rtx-1",
		"gpu_info": []map[string]any{
			{"index": 0, "model": "RTX 4090", "vram_mb": 24576},
		},
	})
	resp, err := http.Post(srv.URL+"/v1/nodes/pair", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got struct {
		NodeID    string `json:"node_id"`
		NodeToken string `json:"node_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.NodeID)
	require.NotEmpty(t, got.NodeToken)
}

func TestPairHandler_BadToken(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"token": "FX-bogus", "name": "n1"})
	resp, err := http.Post(srv.URL+"/v1/nodes/pair", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
