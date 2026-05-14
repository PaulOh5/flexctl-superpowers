package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
)

func mountWithOrigin(t *testing.T, allowed []string) http.Handler {
	t.Helper()
	mw := auth.RequireSameOrigin(allowed)
	return mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestSameOrigin_GETAlwaysAllowed(t *testing.T) {
	h := mountWithOrigin(t, []string{"https://flexctl.example.com"})
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestSameOrigin_PostRejectedWhenOriginMismatch(t *testing.T) {
	h := mountWithOrigin(t, []string{"https://flexctl.example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/envs", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSameOrigin_PostAllowedWhenOriginMatch(t *testing.T) {
	h := mountWithOrigin(t, []string{"https://flexctl.example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/envs", nil)
	req.Header.Set("Origin", "https://flexctl.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestSameOrigin_EmptyAllowedSkipsCheck(t *testing.T) {
	h := mountWithOrigin(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/envs", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "dev mode should skip when allowed list empty")
}

func TestSameOrigin_NoOriginHeaderAllowed(t *testing.T) {
	h := mountWithOrigin(t, []string{"https://flexctl.example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/envs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
