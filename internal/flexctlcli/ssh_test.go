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

func TestSSH_BuildsArgsWithProxyCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "running"},
		})
	}))
	defer srv.Close()
	cfgPath := writeClientCfg(t, srv.URL)

	args, err := flexctlcli.BuildSSHArgs(context.Background(), cfgPath, "/usr/bin/flexctl", "vllm", []string{"-L", "8888:localhost:8888"})
	require.NoError(t, err)
	require.Contains(t, args, "dev@paul-vllm.flex")
	require.Contains(t, args, "-L")
	require.Contains(t, args, "8888:localhost:8888")
	// ProxyCommand option must be present
	var found bool
	for i, a := range args {
		if a == "-o" && i+1 < len(args) && args[i+1] == "ProxyCommand=/usr/bin/flexctl proxy %h %p" {
			found = true
		}
	}
	require.True(t, found, "ProxyCommand option missing: %v", args)
}

func TestSSH_RejectsStoppedEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "stopped"},
		})
	}))
	defer srv.Close()
	cfgPath := writeClientCfg(t, srv.URL)

	_, err := flexctlcli.BuildSSHArgs(context.Background(), cfgPath, "/usr/bin/flexctl", "vllm", nil)
	require.ErrorContains(t, err, "stopped")
}
