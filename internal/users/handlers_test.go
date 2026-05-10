package users_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/users"
)

func TestSignupHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)

	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "p@example.com", "slug": "paul", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Len(t, resp.Cookies(), 1)
	require.Equal(t, "flex_session", resp.Cookies()[0].Name)

	var got map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "paul", got["slug"])
}

func TestSignupHandler_DuplicateReturns409(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	_, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "P@example.com", "slug": "paul2", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}
