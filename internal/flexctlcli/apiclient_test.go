package flexctlcli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestAPIClient_LoginSetsCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/login" {
			http.Error(w, "not found", 404); return
		}
		http.SetCookie(w, &http.Cookie{Name: "flex_session", Value: "abc123"})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "")
	cookie, err := c.Login(context.Background(), "p@x.com", "supersecret")
	require.NoError(t, err)
	require.Equal(t, "flex_session=abc123", cookie)
}

func TestAPIClient_MeReturnsUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/me", r.URL.Path)
		require.Equal(t, "flex_session=abc", r.Header.Get("Cookie"))
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "uid-1", "slug": "paul", "email": "p@x.com"})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	me, err := c.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, "paul", me.Slug)
}

func TestAPIClient_PairDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/devices/pair", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_id": "did", "hostname": "paul-device-mac",
			"preauth_key": "pk", "headscale_url": "https://hs", "tailnet_domain": "flex",
		})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	res, err := c.PairDevice(context.Background(), "mac")
	require.NoError(t, err)
	require.Equal(t, "paul-device-mac", res.Hostname)
	require.Equal(t, "pk", res.PreauthKey)
}

func TestAPIClient_AddSSHKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/me/ssh-keys", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "k1", "fingerprint": "fp"})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	id, err := c.AddSSHKey(context.Background(), "laptop-ed25519", "ssh-ed25519 AAA...")
	require.NoError(t, err)
	require.Equal(t, "k1", id)
}

func TestAPIClient_ListEnvs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/envs", r.URL.Path)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "running"},
		})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	envs, err := c.ListEnvs(context.Background())
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, "vllm", envs[0].Name)
}
