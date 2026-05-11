package flexctlcli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestJoinCommand_PersistsConfig(t *testing.T) {
	// Stub control plane that returns a fixed pair response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/nodes/pair", r.URL.Path)
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, "FX-test-token", req["token"])
		require.Equal(t, "test-node", req["name"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"node_id":    "00000000-0000-0000-0000-000000000123",
			"node_token": "deadbeefcafe",
		})
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	cfgPath := filepath.Join(tmpHome, "agent.toml")

	cmd := flexctlcli.NewJoinCmd()
	cmd.SetArgs([]string{
		"FX-test-token",
		"--name", "test-node",
		"--control-plane", srv.URL,
		"--config", cfgPath,
	})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.NoError(t, err)

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	contents := string(data)
	require.Contains(t, contents, `node_id = "00000000-0000-0000-0000-000000000123"`)
	require.Contains(t, contents, `node_token = "deadbeefcafe"`)
	require.Contains(t, contents, `control_plane = "`+srv.URL+`"`)
}

func TestJoinCommand_FailsOnBadToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid pair token"}`))
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	cfgPath := filepath.Join(tmpHome, "agent.toml")

	cmd := flexctlcli.NewJoinCmd()
	cmd.SetArgs([]string{
		"FX-bad",
		"--name", "test-node",
		"--control-plane", srv.URL,
		"--config", cfgPath,
	})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)

	_, err = os.Stat(cfgPath)
	require.True(t, os.IsNotExist(err), "config must not be written on auth failure")
}

// Compile-time check that NewJoinCmd returns a *cobra.Command.
var _ = (*cobra.Command)(nil)
