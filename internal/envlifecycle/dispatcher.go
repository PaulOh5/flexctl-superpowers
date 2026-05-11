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

func (d *Dispatcher) HandleStop(ctx context.Context, cmd *agentpb.StopEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id")
		return nil
	}
	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()
	if err := d.docker.StopContainer(ctx, devName+"-id", 10*time.Second); err != nil {
		slog.Warn("stop dev", "err", err)
	}
	if err := d.docker.StopContainer(ctx, sidecarName+"-id", 10*time.Second); err != nil {
		slog.Warn("stop sidecar", "err", err)
	}
	d.alloc.Release(envID)
	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvStopped{
			EnvStopped: &agentpb.EnvStopped{EnvId: cmd.GetEnvId()},
		},
	})
	return nil
}

func (d *Dispatcher) HandleStart(ctx context.Context, cmd *agentpb.StartEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id")
		return nil
	}
	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()

	// Inspect existing dev container to recover labels (image_ref, gpu_indices, etc.)
	devInfo, err := d.docker.InspectContainer(ctx, devName+"-id")
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "dev container not found: "+err.Error())
		return nil
	}
	gpuIndices := parseIndicesCSV(devInfo.Labels[envdocker.LabelGPUIndices])

	sidecarInfo, err := d.docker.InspectContainer(ctx, sidecarName+"-id")
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "sidecar container not found: "+err.Error())
		return nil
	}

	// Reallocate GPU — use newly allocated indices so the dev container and
	// sendReady reflect the actual current allocation, not a potentially stale label.
	if len(gpuIndices) > 0 {
		allocated, err := d.alloc.Allocate(envID, len(gpuIndices))
		if err != nil {
			d.sendError(cmd.GetEnvId(), "start", err.Error())
			return nil
		}
		gpuIndices = allocated
	}

	// Recreate sidecar with new preauth_key (docker start can't change ENV)
	if err := d.docker.RemoveContainer(ctx, sidecarName+"-id", true); err != nil {
		d.sendError(cmd.GetEnvId(), "start", "rm sidecar: "+err.Error())
		d.alloc.Release(envID)
		return nil
	}
	sidecarID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name:    sidecarName,
		Image:   sidecarInfo.Labels[envdocker.LabelImageRef],
		Labels:  sidecarInfo.Labels,
		CapAdd:  []string{"NET_ADMIN"},
		Devices: []string{"/dev/net/tun"},
		Env: map[string]string{
			"FLEXCTL_HOSTNAME":      sidecarInfo.Labels[envdocker.LabelHostname],
			"FLEXCTL_AUTHKEY":       cmd.GetPreauthKey(),
			"FLEXCTL_HEADSCALE_URL": sidecarInfo.Labels[envdocker.LabelHeadscaleURL],
			"FLEXCTL_TAGS":          sidecarInfo.Labels[envdocker.LabelTags],
		},
	})
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "create sidecar: "+err.Error())
		d.alloc.Release(envID)
		return nil
	}
	if err := d.docker.StartContainer(ctx, sidecarID); err != nil {
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "start", "start sidecar: "+err.Error())
		return nil
	}
	if err := d.waitSidecarHealth(ctx, sidecarID); err != nil {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "start", "sidecar health: "+err.Error())
		return nil
	}

	// Recreate dev with new authorized_keys.
	// On any failure here the new sidecar must also be cleaned up.
	abortSidecar := func() {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
	}
	if err := d.docker.RemoveContainer(ctx, devName+"-id", true); err != nil {
		abortSidecar()
		d.sendError(cmd.GetEnvId(), "start", "rm dev: "+err.Error())
		return nil
	}
	devID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: devName, Image: devInfo.Labels[envdocker.LabelImageRef],
		Labels:      devInfo.Labels,
		NetworkMode: "container:" + sidecarName,
		GPUIndices:  gpuIndices,
		Env:         map[string]string{"FLEXCTL_AUTHORIZED_KEYS": cmd.GetAuthorizedKeys()},
		VolumeMounts: []envdocker.VolumeMount{
			{Source: devInfo.Labels[envdocker.LabelVolumeName], Target: "/home/dev"},
		},
	})
	if err != nil {
		abortSidecar()
		d.sendError(cmd.GetEnvId(), "start", "create dev: "+err.Error())
		return nil
	}
	if err := d.docker.StartContainer(ctx, devID); err != nil {
		_ = d.docker.RemoveContainer(ctx, devID, true)
		abortSidecar()
		d.sendError(cmd.GetEnvId(), "start", "start dev: "+err.Error())
		return nil
	}

	d.sendReady(cmd.GetEnvId(), sidecarID, devID, gpuIndices)
	return nil
}

func (d *Dispatcher) HandleDelete(ctx context.Context, cmd *agentpb.DeleteEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id")
		return nil
	}
	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()
	volumeName := "flex-env-" + cmd.GetEnvId()

	_ = d.docker.StopContainer(ctx, devName+"-id", 5*time.Second)
	_ = d.docker.StopContainer(ctx, sidecarName+"-id", 5*time.Second)
	_ = d.docker.RemoveContainer(ctx, devName+"-id", true)
	_ = d.docker.RemoveContainer(ctx, sidecarName+"-id", true)
	if err := d.docker.RemoveVolume(ctx, volumeName); err != nil {
		slog.Warn("rm volume", "err", err, "name", volumeName)
	}
	d.alloc.Release(envID)

	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvDeleted{
			EnvDeleted: &agentpb.EnvDeleted{EnvId: cmd.GetEnvId()},
		},
	})
	return nil
}

// BuildEnvStateSnapshot lists running dev containers and returns their env_ids.
// Called by flexctlagent on reconnect (sent as EnvStateSnapshot).
func (d *Dispatcher) BuildEnvStateSnapshot(ctx context.Context) ([]string, error) {
	list, err := d.docker.ListContainers(ctx, map[string]string{
		envdocker.LabelRole: "dev",
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		if id := c.Labels[envdocker.LabelEnvID]; id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}
