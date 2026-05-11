package envs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/envs"
)

type fakeDispatcher struct {
	mu       sync.Mutex
	creates  []uuid.UUID
	stops    []uuid.UUID
	starts   []uuid.UUID
	deletes  []uuid.UUID
	failNext error
}

func (f *fakeDispatcher) Create(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock()
	if f.failNext != nil { e := f.failNext; f.failNext = nil; return e }
	f.creates = append(f.creates, envID); return nil
}
func (f *fakeDispatcher) Stop(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.stops = append(f.stops, envID); return nil
}
func (f *fakeDispatcher) Start(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.starts = append(f.starts, envID); return nil
}
func (f *fakeDispatcher) Delete(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.deletes = append(f.deletes, envID); return nil
}

func mountAuthed(t *testing.T, signer *auth.SessionSigner, h *envs.Handlers) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.Mount(r)
	})
	return httptest.NewServer(r)
}

func TestPostEnvs_HappyPath(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()

	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id":     nid.String(),
		"template_id": "cuda-base",
		"name":        "vllm",
		"gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, disp.creates, 1)
}

func TestPostEnvs_DispatcherFailsRollsBack(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	disp := &fakeDispatcher{failNext: errors.New("agent offline")}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()

	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id": nid.String(), "template_id": "cuda-base", "name": "vllm", "gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	got, _ := svc.ListByOwner(context.Background(), uid)
	require.Len(t, got, 0)
}

func TestPostEnvs_DuplicateName409(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	_, err := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "dup", GPURequest: 1,
	})
	require.NoError(t, err)

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id": nid.String(), "template_id": "cuda-base", "name": "dup", "gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestStopHandler_HappyPath(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs/"+env.ID.String()+"/stop", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Equal(t, []uuid.UUID{env.ID}, disp.stops)
}

func TestStopHandler_NotOwner404(t *testing.T) {
	pool := newPool(t)
	uidA, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uidA, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})

	// User B
	otherID := uuid.New()
	_, _ = pool.Exec(context.Background(),
		`INSERT INTO users (id, email, slug, password_hash) VALUES ($1, 'b@x.com', 'bob', 'x')`, otherID)

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tokB, _ := signer.Encode(auth.Session{UserID: otherID, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs/"+env.ID.String()+"/stop", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tokB})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Empty(t, disp.stops)
}

func TestListEnvs(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	_, _ = svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "alpha", GPURequest: 1,
	})

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/envs", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got, 1)
	require.Equal(t, "alpha", got[0]["name"])
}
