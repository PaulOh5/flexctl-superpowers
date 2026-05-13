package flexctlcli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestSSHConfig_InsertOnEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	block := flexctlcli.SSHConfigBlock{
		FlexctlBinary: "/usr/local/bin/flexctl",
		KnownHosts:    "/home/me/.config/flexctl/known_hosts",
	}
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, block))

	out, _ := os.ReadFile(path)
	s := string(out)
	require.Contains(t, s, "# >>> flexctl >>>")
	require.Contains(t, s, "# <<< flexctl <<<")
	require.Contains(t, s, "Host *.flex")
	require.Contains(t, s, "ProxyCommand /usr/local/bin/flexctl proxy %h %p")
}

func TestSSHConfig_PreservesOtherContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	original := "Host other.example.com\n    User alice\n\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	require.NoError(t, flexctlcli.UpsertSSHConfig(path, flexctlcli.SSHConfigBlock{
		FlexctlBinary: "/usr/local/bin/flexctl",
		KnownHosts:    "/k",
	}))
	out, _ := os.ReadFile(path)
	require.Contains(t, string(out), "Host other.example.com")
	require.Contains(t, string(out), "User alice")
	require.Contains(t, string(out), "# >>> flexctl >>>")
}

func TestSSHConfig_IdempotentReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	b1 := flexctlcli.SSHConfigBlock{FlexctlBinary: "/v1/flexctl", KnownHosts: "/k"}
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, b1))
	b2 := flexctlcli.SSHConfigBlock{FlexctlBinary: "/v2/flexctl", KnownHosts: "/k"}
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, b2))

	out, _ := os.ReadFile(path)
	require.NotContains(t, string(out), "/v1/flexctl")
	require.Contains(t, string(out), "/v2/flexctl")
	require.Equal(t, 1, strings.Count(string(out), "# >>> flexctl >>>"))
}

func TestSSHConfig_Remove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, flexctlcli.SSHConfigBlock{FlexctlBinary: "/x", KnownHosts: "/k"}))

	require.NoError(t, flexctlcli.RemoveSSHConfigBlock(path))
	out, _ := os.ReadFile(path)
	require.NotContains(t, string(out), "# >>> flexctl >>>")
	require.NotContains(t, string(out), "ProxyCommand")
}

func TestSSHConfig_PermsAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(path, []byte("Host x\n"), 0o600))

	require.NoError(t, flexctlcli.UpsertSSHConfig(path, flexctlcli.SSHConfigBlock{FlexctlBinary: "/x", KnownHosts: "/k"}))

	info, _ := os.Stat(path)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	matches, _ := filepath.Glob(path + ".flexctl-bak.*")
	require.Len(t, matches, 1, "expected exactly one backup file")
}
