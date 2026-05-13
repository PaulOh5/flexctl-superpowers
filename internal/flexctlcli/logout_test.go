package flexctlcli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestLogout_DeletesDeviceAndCleansFiles(t *testing.T) {
	var (
		deletedDevice atomic.Bool
		loggedOut     atomic.Bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/devices/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletedDevice.Store(true)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		loggedOut.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/devices", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"did","name":"mac","hostname":"paul-device-mac"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".config", "flexctl", "client.toml")
	t.Setenv("FLEXCTL_CLIENT_CONFIG", cfgPath)
	t.Setenv("FLEXCTL_STATE_DIR", filepath.Join(home, ".config", "flexctl", "tsnet"))
	sshCfg := filepath.Join(home, ".ssh", "config")
	t.Setenv("FLEXCTL_SSH_CONFIG", sshCfg)

	require.NoError(t, flexctlcli.WriteClientConfig(cfgPath, flexctlcli.ClientConfig{
		ControlPlane: srv.URL, SessionCookie: "flex_session=abc",
		DeviceName: "mac", DeviceHostname: "paul-device-mac",
	}))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "flexctl", "tsnet"), 0o700))
	require.NoError(t, flexctlcli.UpsertSSHConfig(sshCfg, flexctlcli.SSHConfigBlock{FlexctlBinary: "/x", KnownHosts: "/k"}))

	cmd := flexctlcli.NewLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))

	require.True(t, deletedDevice.Load())
	require.True(t, loggedOut.Load())

	_, err := os.Stat(cfgPath); require.True(t, os.IsNotExist(err))
	body, _ := os.ReadFile(sshCfg)
	require.NotContains(t, string(body), "# >>> flexctl >>>")
}
