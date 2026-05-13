package flexctlcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestEnvList_PrintsTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "running", "node_id": "gpu-01"},
			{"id": "e2", "name": "sd",   "hostname": "paul-sd",   "status": "stopped", "node_id": "gpu-01"},
		})
	}))
	defer srv.Close()
	cfgPath := writeClientCfg(t, srv.URL)

	cmd := flexctlcli.NewEnvCmd()
	cmd.SetArgs([]string{"list", "--config", cfgPath})
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	s := out.String()
	require.Contains(t, s, "vllm")
	require.Contains(t, s, "paul-vllm")
	require.Contains(t, s, "running")
	require.Contains(t, s, "sd")
	require.Contains(t, s, "stopped")
}
