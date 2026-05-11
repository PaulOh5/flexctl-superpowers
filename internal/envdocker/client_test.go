package envdocker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/envdocker"
)

func TestMockDockerClient_HappyPath(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	ctx := context.Background()

	id, err := m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-test", Image: "busybox", Labels: map[string]string{
			envdocker.LabelEnvID: "abc", envdocker.LabelRole: "dev",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "flex-env-test-id", id)
	require.NoError(t, m.StartContainer(ctx, id))

	got, err := m.InspectContainer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "running", got.State)
	require.Equal(t, "abc", got.Labels[envdocker.LabelEnvID])
}

func TestMockDockerClient_FilterByLabel(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	ctx := context.Background()
	_, _ = m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "a", Labels: map[string]string{envdocker.LabelRole: "sidecar"},
	})
	_, _ = m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "b", Labels: map[string]string{envdocker.LabelRole: "dev"},
	})

	got, err := m.ListContainers(ctx, map[string]string{envdocker.LabelRole: "dev"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "b", got[0].Name)
}

func TestMockDockerClient_InjectError(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	m.NextErrors["CreateContainer:flex-env-x"] = errors.New("pull failed")

	_, err := m.CreateContainer(context.Background(), envdocker.ContainerSpec{Name: "flex-env-x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pull failed")

	_, err = m.CreateContainer(context.Background(), envdocker.ContainerSpec{Name: "flex-env-y"})
	require.NoError(t, err)
}

func TestMockDockerClient_SetExecOutputForRole(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	ctx := context.Background()
	_, _ = m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-net-a", Labels: map[string]string{envdocker.LabelRole: "sidecar"},
	})
	_, _ = m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-a", Labels: map[string]string{envdocker.LabelRole: "dev"},
	})

	m.SetExecOutputForRole("sidecar", "tailscale 100.64.0.5\n")

	stdout, _, _, err := m.Exec(ctx, "flex-net-a-id", []string{"tailscale", "status"})
	require.NoError(t, err)
	buf := make([]byte, 100)
	n, _ := stdout.Read(buf)
	require.Contains(t, string(buf[:n]), "100.64.0.5")

	// dev container should have empty stdout (no SetExecOutput for it)
	stdout2, _, _, _ := m.Exec(ctx, "flex-env-a-id", []string{"echo"})
	buf2 := make([]byte, 100)
	n2, _ := stdout2.Read(buf2)
	require.Equal(t, 0, n2)
}
