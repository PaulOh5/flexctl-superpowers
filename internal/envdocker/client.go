package envdocker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

type ContainerSpec struct {
	Name         string
	Image        string
	Cmd          []string
	Env          map[string]string
	Labels       map[string]string
	NetworkMode  string
	CapAdd       []string
	Devices      []string
	GPUIndices   []int
	VolumeMounts []VolumeMount
}

type VolumeMount struct {
	Source string
	Target string
}

type ContainerInfo struct {
	ID     string
	Name   string
	State  string
	Labels map[string]string
	Env    []string
}

type DockerClient interface {
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (ContainerInfo, error)
	ListContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerInfo, error)
	Exec(ctx context.Context, id string, cmd []string) (stdout, stderr io.Reader, exitCode int, err error)
	RemoveVolume(ctx context.Context, name string) error
	PullImage(ctx context.Context, ref string) error
}

type RealDockerClient struct {
	cli *client.Client
}

func NewRealDockerClient() (*RealDockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &RealDockerClient{cli: cli}, nil
}

func (r *RealDockerClient) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	cfg := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Cmd,
		Labels: spec.Labels,
		Env:    envMapToSlice(spec.Env),
	}
	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.NetworkMode),
		CapAdd:      spec.CapAdd,
	}
	for _, d := range spec.Devices {
		hostCfg.Devices = append(hostCfg.Devices, container.DeviceMapping{
			PathOnHost: d, PathInContainer: d, CgroupPermissions: "rwm",
		})
	}
	for _, m := range spec.VolumeMounts {
		hostCfg.Binds = append(hostCfg.Binds, m.Source+":"+m.Target)
	}
	if len(spec.GPUIndices) > 0 {
		ids := make([]string, 0, len(spec.GPUIndices))
		for _, i := range spec.GPUIndices {
			ids = append(ids, strconv.Itoa(i))
		}
		hostCfg.Resources.DeviceRequests = []container.DeviceRequest{{
			Driver:       "nvidia",
			Capabilities: [][]string{{"gpu"}},
			DeviceIDs:    ids,
		}}
	}
	resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	return resp.ID, nil
}

func (r *RealDockerClient) StartContainer(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *RealDockerClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &secs})
}

func (r *RealDockerClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

func (r *RealDockerClient) InspectContainer(ctx context.Context, id string) (ContainerInfo, error) {
	j, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("inspect: %w", err)
	}
	return ContainerInfo{
		ID:     j.ID,
		Name:   j.Name,
		State:  j.State.Status,
		Labels: j.Config.Labels,
		Env:    j.Config.Env,
	}, nil
}

func (r *RealDockerClient) ListContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerInfo, error) {
	args := filters.NewArgs()
	for k, v := range labelFilter {
		args.Add("label", k+"="+v)
	}
	list, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		out = append(out, ContainerInfo{
			ID: c.ID, Name: name, State: c.State, Labels: c.Labels,
		})
	}
	return out, nil
}

func (r *RealDockerClient) Exec(ctx context.Context, id string, cmd []string) (io.Reader, io.Reader, int, error) {
	exec, err := r.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("exec create: %w", err)
	}
	att, err := r.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	if _, err := io.Copy(&stdout, att.Reader); err != nil {
		return nil, nil, 0, fmt.Errorf("read stdout: %w", err)
	}
	insp, err := r.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("exec inspect: %w", err)
	}
	return &stdout, &stderr, insp.ExitCode, nil
}

func (r *RealDockerClient) RemoveVolume(ctx context.Context, name string) error {
	return r.cli.VolumeRemove(ctx, name, false)
}

func (r *RealDockerClient) PullImage(ctx context.Context, ref string) error {
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func envMapToSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
