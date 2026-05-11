package envlifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
)

// stubStream collects AgentMessages sent by the dispatcher.
type stubStream struct {
	sent []*agentpb.AgentMessage
}

func (s *stubStream) Send(msg *agentpb.AgentMessage) error {
	s.sent = append(s.sent, proto.Clone(msg).(*agentpb.AgentMessage))
	return nil
}

func newDispatcher(t *testing.T, m envdocker.DockerClient, sender envlifecycle.AgentSender) *envlifecycle.Dispatcher {
	t.Helper()
	alloc := envlifecycle.NewGPUAllocator([]int{0, 1, 2, 3})
	return envlifecycle.NewDispatcher(m, alloc, sender)
}

func TestDispatcher_CreateEnv_HappyPath(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	cmd := &agentpb.CreateEnv{
		EnvId:           envID.String(),
		ImageRef:        "flex/dev-cuda-base:dev",
		SidecarImageRef: "flex/sidecar:dev",
		Hostname:        "paul-vllm",
		HeadscaleUrl:    "http://headscale:8080",
		PreauthKey:      "ts-key-1",
		Tags:            []string{"tag:env-paul"},
		AuthorizedKeys:  "ssh-ed25519 AAAA...",
		GpuRequest:      2,
	}

	// Pre-arm sidecar health check
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = "tailscale 100.64.0.5\n"

	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	got, _ := mock.ListContainers(context.Background(), nil)
	require.Len(t, got, 2)

	require.Len(t, stub.sent, 1)
	ack := stub.sent[0].GetEnvReady()
	require.NotNil(t, ack)
	require.Equal(t, envID.String(), ack.EnvId)
	require.Equal(t, []int32{0, 1}, ack.GpuIndices)
}

func TestDispatcher_CreateEnv_InsufficientGPU(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	alloc := envlifecycle.NewGPUAllocator([]int{0}) // 1 GPU only
	d := envlifecycle.NewDispatcher(mock, alloc, stub)

	envID := uuid.New()
	cmd := &agentpb.CreateEnv{
		EnvId:           envID.String(),
		ImageRef:        "flex/dev-cuda-base:dev",
		SidecarImageRef: "flex/sidecar:dev",
		Hostname:        "paul-x",
		PreauthKey:      "ts-key-2",
		Tags:            []string{"tag:env-paul"},
		GpuRequest:      2, // exceeds available
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	got, _ := mock.ListContainers(context.Background(), nil)
	require.Empty(t, got)

	require.Len(t, stub.sent, 1)
	errMsg := stub.sent[0].GetEnvError()
	require.NotNil(t, errMsg)
	require.Equal(t, "unknown", errMsg.Stage)
	require.Contains(t, errMsg.Detail, "insufficient")
}

func TestDispatcher_CreateEnv_SidecarStartFails_CleansUp(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	envID := uuid.New()
	mock.NextErrors["StartContainer:flex-net-"+envID.String()+"-id"] = errors.New("boom")

	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	cmd := &agentpb.CreateEnv{
		EnvId: envID.String(), ImageRef: "x", SidecarImageRef: "y",
		Hostname: "h", PreauthKey: "k", GpuRequest: 1,
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	require.Contains(t, mock.Calls, "RemoveContainer:flex-net-"+envID.String()+"-id")

	require.Len(t, stub.sent, 1)
	errMsg := stub.sent[0].GetEnvError()
	require.NotNil(t, errMsg)
	require.Equal(t, "sidecar_start", errMsg.Stage)
}

func TestDispatcher_CreateEnv_SidecarHealthTimeout(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	alloc := envlifecycle.NewGPUAllocator([]int{0, 1})
	d := envlifecycle.NewDispatcherWithTimeout(mock, alloc, stub, 200*time.Millisecond)

	envID := uuid.New()
	// Exec returns empty → never online
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = ""

	cmd := &agentpb.CreateEnv{
		EnvId: envID.String(), ImageRef: "x", SidecarImageRef: "y",
		Hostname: "h", PreauthKey: "k", GpuRequest: 1,
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	require.Len(t, stub.sent, 1)
	errMsg := stub.sent[0].GetEnvError()
	require.NotNil(t, errMsg)
	require.Equal(t, "sidecar_health", errMsg.Stage)
}

// helper: create env via HandleCreate to set up state
func setupRunningEnv(t *testing.T, d *envlifecycle.Dispatcher, mock *envdocker.MockDockerClient, envID uuid.UUID) {
	t.Helper()
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = "tailscale 100.64.0.5\n"
	cmd := &agentpb.CreateEnv{
		EnvId: envID.String(), ImageRef: "flex/dev:dev", SidecarImageRef: "flex/sidecar:dev",
		Hostname: "h", PreauthKey: "k", GpuRequest: 1,
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))
}

func TestDispatcher_StopEnv(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	stub.sent = nil // reset

	require.NoError(t, d.HandleStop(context.Background(), &agentpb.StopEnv{EnvId: envID.String()}))

	require.Len(t, stub.sent, 1)
	require.NotNil(t, stub.sent[0].GetEnvStopped())
	require.Equal(t, envID.String(), stub.sent[0].GetEnvStopped().EnvId)

	infoDev, _ := mock.InspectContainer(context.Background(), "flex-env-"+envID.String()+"-id")
	require.Equal(t, "exited", infoDev.State)
}

func TestDispatcher_StartEnv(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	require.NoError(t, d.HandleStop(context.Background(), &agentpb.StopEnv{EnvId: envID.String()}))
	stub.sent = nil

	// Re-arm health for restart's recreated sidecar
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = "tailscale 100.64.0.5\n"
	require.NoError(t, d.HandleStart(context.Background(), &agentpb.StartEnv{
		EnvId: envID.String(), PreauthKey: "new-key", AuthorizedKeys: "ssh-ed25519 new",
	}))

	require.Len(t, stub.sent, 1)
	require.NotNil(t, stub.sent[0].GetEnvReady())
}

func TestDispatcher_DeleteEnv(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	stub.sent = nil

	require.NoError(t, d.HandleDelete(context.Background(), &agentpb.DeleteEnv{EnvId: envID.String()}))

	require.Len(t, stub.sent, 1)
	require.NotNil(t, stub.sent[0].GetEnvDeleted())

	all, _ := mock.ListContainers(context.Background(), nil)
	require.Empty(t, all)
}

func TestDispatcher_DeleteEnv_VolumeRemoved(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	mock.Volumes["flex-env-pre"] = true
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	mock.Volumes["flex-env-"+envID.String()] = true
	stub.sent = nil

	require.NoError(t, d.HandleDelete(context.Background(), &agentpb.DeleteEnv{EnvId: envID.String()}))

	require.False(t, mock.Volumes["flex-env-"+envID.String()])
	require.True(t, mock.Volumes["flex-env-pre"])
}

func TestDispatcher_BuildEnvStateSnapshot(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)

	snap, err := d.BuildEnvStateSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{envID.String()}, snap)
}
