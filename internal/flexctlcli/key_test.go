package flexctlcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func writeClientCfg(t *testing.T, base string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := flexctlcli.ClientConfig{
		ControlPlane: base, SessionCookie: "flex_session=ok",
	}
	p := filepath.Join(dir, "client.toml")
	require.NoError(t, flexctlcli.WriteClientConfig(p, cfg))
	return p
}

func TestKeyAdd_ReadsFileAndPOSTs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/me/ssh-keys", r.URL.Path)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		require.Equal(t, "ssh-ed25519 AAAA test-key", body["public_key"])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "k1", "fingerprint": "fp"})
	}))
	defer srv.Close()

	pubPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	require.NoError(t, os.WriteFile(pubPath, []byte("ssh-ed25519 AAAA test-key\n"), 0o600))
	cfgPath := writeClientCfg(t, srv.URL)

	cmd := flexctlcli.NewKeyCmd()
	cmd.SetArgs([]string{"add", pubPath, "--name", "lp", "--config", cfgPath})
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Contains(t, out.String(), "k1")
}
