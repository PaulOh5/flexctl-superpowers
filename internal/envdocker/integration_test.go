//go:build integration

package envdocker_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/envdocker"
)

func TestRealDocker_AlpineLifecycle(t *testing.T) {
	c, err := envdocker.NewRealDockerClient()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, c.PullImage(ctx, "alpine:3.20"))

	name := "flex-envdocker-integration-" + time.Now().Format("150405")
	id, err := c.CreateContainer(ctx, envdocker.ContainerSpec{
		Name:  name,
		Image: "alpine:3.20",
		Cmd:   []string{"sleep", "10"},
		Labels: map[string]string{
			envdocker.LabelEnvID: "integration-test",
			envdocker.LabelRole:  "dev",
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id, true) })

	require.NoError(t, c.StartContainer(ctx, id))

	info, err := c.InspectContainer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "running", info.State)
	require.Equal(t, "integration-test", info.Labels[envdocker.LabelEnvID])

	stdout, _, exit, err := c.Exec(ctx, id, []string{"echo", "hello"})
	require.NoError(t, err)
	require.Equal(t, 0, exit)
	buf, _ := io.ReadAll(stdout)
	require.Contains(t, string(buf), "hello")

	list, err := c.ListContainers(ctx, map[string]string{envdocker.LabelEnvID: "integration-test"})
	require.NoError(t, err)
	require.NotEmpty(t, list)

	require.NoError(t, c.StopContainer(ctx, id, 5*time.Second))
	require.NoError(t, c.RemoveContainer(ctx, id, true))
}
