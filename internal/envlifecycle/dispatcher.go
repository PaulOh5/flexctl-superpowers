package envlifecycle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/envdocker"
)

// AgentSender abstracts the agent → control-plane stream send so the dispatcher
// can be tested without a real gRPC stream.
type AgentSender interface {
	Send(msg *agentpb.AgentMessage) error
}

type Dispatcher struct {
	docker        envdocker.DockerClient
	alloc         *GPUAllocator
	sender        AgentSender
	healthTimeout time.Duration
}

func NewDispatcher(docker envdocker.DockerClient, alloc *GPUAllocator, sender AgentSender) *Dispatcher {
	return NewDispatcherWithTimeout(docker, alloc, sender, 30*time.Second)
}

func NewDispatcherWithTimeout(docker envdocker.DockerClient, alloc *GPUAllocator, sender AgentSender, healthTimeout time.Duration) *Dispatcher {
	return &Dispatcher{docker: docker, alloc: alloc, sender: sender, healthTimeout: healthTimeout}
}

func (d *Dispatcher) sendError(envID, stage, detail string) {
	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvError{
			EnvError: &agentpb.EnvError{EnvId: envID, Stage: stage, Detail: detail},
		},
	})
}

func (d *Dispatcher) sendReady(envID, sidecarID, devID string, gpuIdx []int) {
	idx32 := make([]int32, 0, len(gpuIdx))
	for _, i := range gpuIdx {
		idx32 = append(idx32, int32(i))
	}
	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvReady{
			EnvReady: &agentpb.EnvReady{
				EnvId: envID, SidecarContainerId: sidecarID, DevContainerId: devID, GpuIndices: idx32,
			},
		},
	})
}

func (d *Dispatcher) HandleCreate(ctx context.Context, cmd *agentpb.CreateEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id: "+err.Error())
		return nil
	}

	// Allocate GPU
	indices, err := d.alloc.Allocate(envID, int(cmd.GetGpuRequest()))
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", err.Error())
		return nil
	}

	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()

	// Create sidecar
	sidecarLabels := map[string]string{
		envdocker.LabelEnvID:        cmd.GetEnvId(),
		envdocker.LabelRole:         "sidecar",
		envdocker.LabelHostname:     cmd.GetHostname(),
		envdocker.LabelHeadscaleURL: cmd.GetHeadscaleUrl(),
		envdocker.LabelTags:         strings.Join(cmd.GetTags(), ","),
		envdocker.LabelImageRef:     cmd.GetSidecarImageRef(),
	}
	sidecarEnv := map[string]string{
		"FLEXCTL_HOSTNAME":      cmd.GetHostname(),
		"FLEXCTL_AUTHKEY":       cmd.GetPreauthKey(),
		"FLEXCTL_HEADSCALE_URL": cmd.GetHeadscaleUrl(),
		"FLEXCTL_TAGS":          strings.Join(cmd.GetTags(), ","),
	}
	sidecarID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: sidecarName, Image: cmd.GetSidecarImageRef(),
		Env: sidecarEnv, Labels: sidecarLabels,
		CapAdd:  []string{"NET_ADMIN"},
		Devices: []string{"/dev/net/tun"},
	})
	if err != nil {
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "sidecar_start", err.Error())
		return nil
	}

	// Start sidecar
	if err := d.docker.StartContainer(ctx, sidecarID); err != nil {
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "sidecar_start", err.Error())
		return nil
	}

	// Wait for sidecar health
	if err := d.waitSidecarHealth(ctx, sidecarID); err != nil {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "sidecar_health", err.Error())
		return nil
	}

	// Create dev container
	devLabels := map[string]string{
		envdocker.LabelEnvID:      cmd.GetEnvId(),
		envdocker.LabelRole:       "dev",
		envdocker.LabelImageRef:   cmd.GetImageRef(),
		envdocker.LabelGPUIndices: IndicesCSV(indices),
		envdocker.LabelVolumeName: "flex-env-" + cmd.GetEnvId(),
	}
	devEnv := map[string]string{
		"FLEXCTL_AUTHORIZED_KEYS": cmd.GetAuthorizedKeys(),
	}
	devID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: devName, Image: cmd.GetImageRef(), Cmd: cmd.GetDefaultCmd(),
		Env: devEnv, Labels: devLabels,
		NetworkMode: "container:" + sidecarName,
		GPUIndices:  indices,
		VolumeMounts: []envdocker.VolumeMount{
			{Source: "flex-env-" + cmd.GetEnvId(), Target: "/home/dev"},
		},
	})
	if err != nil {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "dev_start", err.Error())
		return nil
	}

	if err := d.docker.StartContainer(ctx, devID); err != nil {
		_ = d.docker.RemoveContainer(ctx, devID, true)
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "dev_start", err.Error())
		return nil
	}

	d.sendReady(cmd.GetEnvId(), sidecarID, devID, indices)
	return nil
}

// waitSidecarHealth polls `tailscale status` until it reports an IP, or times out.
// Considers any non-empty stdout containing "100." (tailnet CGNAT prefix) as healthy.
func (d *Dispatcher) waitSidecarHealth(ctx context.Context, sidecarID string) error {
	deadline := time.Now().Add(d.healthTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		stdout, _, _, err := d.docker.Exec(ctx, sidecarID, []string{"tailscale", "status"})
		if err == nil {
			buf, _ := io.ReadAll(stdout)
			if strings.Contains(string(buf), "100.") {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("sidecar did not report tailnet IP in %s", d.healthTimeout)
}

// no-op for slog import until Task 11 expands this file
var _ = slog.Default
