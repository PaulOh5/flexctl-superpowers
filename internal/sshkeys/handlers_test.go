package sshkeys_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

func mountAuthed(t *testing.T, signer *auth.SessionSigner, svc *sshkeys.Service) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		sshkeys.NewHandlers(svc).Mount(r)
	})
	return httptest.NewServer(r)
}

func TestAddKeyHandler(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, sshkeys.NewService(pool))
	defer srv.Close()
	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})

	body, _ := json.Marshal(map[string]string{"name": "laptop", "public_key": validKey})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/me/ssh-keys", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestAddKeyHandler_BadKey400(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, sshkeys.NewService(pool))
	defer srv.Close()
	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})

	body, _ := json.Marshal(map[string]string{"name": "x", "public_key": "garbage"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/me/ssh-keys", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestListKeysHandler_Empty(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, sshkeys.NewService(pool))
	defer srv.Close()
	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me/ssh-keys", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got []map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got, 0)
}

func TestDeleteKeyHandler_OtherUser404(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	a, _ := usersSvc.Signup(context.Background(), "a@x.com", "alice", "correct-horse-battery")
	b, _ := usersSvc.Signup(context.Background(), "b@x.com", "bob", "correct-horse-battery")

	svc := sshkeys.NewService(pool)
	k, _ := svc.Add(context.Background(), a.ID, "alice-key", validKey)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, svc)
	defer srv.Close()
	tokenB, _ := signer.Encode(auth.Session{UserID: b.ID, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/me/ssh-keys/"+k.ID.String(), nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tokenB})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
