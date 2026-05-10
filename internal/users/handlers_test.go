package users_test

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
	"github.com/paul/flexctl/internal/users"
)

func TestSignupHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)

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
	h := users.NewHandlers(svc, signer, pool)
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

func TestLoginHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	_, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "P@example.com", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, resp.Cookies(), 1)
}

func TestLoginHandler_BadPasswordReturns401(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	_, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "p@example.com", "password": "wrong-password-here",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestLoginHandler_UnknownEmailReturns401(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "nobody@example.com", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func timeNowPlusHour() time.Time { return time.Now().Add(time.Hour) }

func TestMeHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	u, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: timeNowPlusHour()})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "paul", got["slug"])
}

func TestMeHandler_NoCookie401(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/me")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestLogoutHandler_ClearsCookie(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	u, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer, pool)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: timeNowPlusHour()})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Len(t, resp.Cookies(), 1)
	c := resp.Cookies()[0]
	require.Equal(t, "flex_session", c.Name)
	require.Equal(t, "", c.Value)
	require.True(t, c.MaxAge < 0 || c.Expires.Before(time.Now()))
}
