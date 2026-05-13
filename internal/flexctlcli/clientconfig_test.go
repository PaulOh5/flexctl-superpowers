package flexctlcli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestClientConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.toml")

	want := flexctlcli.ClientConfig{
		ControlPlane:    "https://flexctl.example.com",
		SessionCookie:   "flex_session=eyJh...",
		DeviceName:      "macbook",
		DeviceHostname:  "paul-device-macbook",
		HeadscaleURL:    "https://headscale.example.com",
		TailnetDomain:   "flex",
	}
	require.NoError(t, flexctlcli.WriteClientConfig(path, want))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := flexctlcli.ReadClientConfig(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestClientConfig_MissingFileReturnsErrNotExist(t *testing.T) {
	_, err := flexctlcli.ReadClientConfig(filepath.Join(t.TempDir(), "nope.toml"))
	require.True(t, os.IsNotExist(err))
}
