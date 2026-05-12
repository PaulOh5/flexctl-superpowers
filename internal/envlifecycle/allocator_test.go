package envlifecycle_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
)

func TestAllocator_AllocateZero(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	idx, err := a.Allocate(uuid.New(), 0)
	require.NoError(t, err)
	require.Empty(t, idx)
}

func TestAllocator_AllocateSequential(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1, 2, 3})
	e1, e2 := uuid.New(), uuid.New()

	i1, err := a.Allocate(e1, 2)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, i1)

	i2, err := a.Allocate(e2, 2)
	require.NoError(t, err)
	require.Equal(t, []int{2, 3}, i2)
}

func TestAllocator_Insufficient(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	_, err := a.Allocate(uuid.New(), 3)
	require.ErrorIs(t, err, envlifecycle.ErrInsufficientGPUs)
}

func TestAllocator_Release(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	e1 := uuid.New()
	_, _ = a.Allocate(e1, 2)
	a.Release(e1)

	e2 := uuid.New()
	idx, err := a.Allocate(e2, 2)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, idx)
}

func TestAllocator_RestoreFromDocker(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1, 2, 3})
	mock := envdocker.NewMockDockerClient()
	ctx := context.Background()
	envID := uuid.New().String()
	_, _ = mock.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-x",
		Labels: map[string]string{
			envdocker.LabelEnvID:      envID,
			envdocker.LabelRole:       "dev",
			envdocker.LabelGPUIndices: "1,3",
		},
	})

	require.NoError(t, a.RestoreFromDocker(ctx, mock))

	idx, err := a.Allocate(uuid.New(), 2)
	require.NoError(t, err)
	require.Equal(t, []int{0, 2}, idx)
}

func TestAllocator_RestoreFromDocker_EmptyIndices(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	mock := envdocker.NewMockDockerClient()
	ctx := context.Background()
	_, _ = mock.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-x",
		Labels: map[string]string{
			envdocker.LabelEnvID:      uuid.New().String(),
			envdocker.LabelRole:       "dev",
			envdocker.LabelGPUIndices: "",
		},
	})
	require.NoError(t, a.RestoreFromDocker(ctx, mock))

	idx, _ := a.Allocate(uuid.New(), 2)
	require.Equal(t, []int{0, 1}, idx)
}
