package envdocker

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// MockDockerClient is a deterministic in-memory implementation for unit tests.
// All methods are safe to call concurrently.
type MockDockerClient struct {
	mu         sync.Mutex
	Containers map[string]ContainerInfo // keyed by ID
	Volumes    map[string]bool
	Calls      []string         // sequence of method names + key
	NextErrors map[string]error // method-key → error to return once
	ExecOutput map[string]string // container ID → stdout for next Exec
	ExecExit   map[string]int   // container ID → exit code
}

func NewMockDockerClient() *MockDockerClient {
	return &MockDockerClient{
		Containers: map[string]ContainerInfo{},
		Volumes:    map[string]bool{},
		NextErrors: map[string]error{},
		ExecOutput: map[string]string{},
		ExecExit:   map[string]int{},
	}
}

func (m *MockDockerClient) take(method string) error {
	m.Calls = append(m.Calls, method)
	if e, ok := m.NextErrors[method]; ok {
		delete(m.NextErrors, method)
		return e
	}
	return nil
}

func (m *MockDockerClient) CreateContainer(_ context.Context, spec ContainerSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("CreateContainer:" + spec.Name); err != nil {
		return "", err
	}
	id := spec.Name
	envSlice := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		envSlice = append(envSlice, k+"="+v)
	}
	m.Containers[id] = ContainerInfo{
		ID: id, Name: spec.Name, State: "created", Labels: spec.Labels, Env: envSlice,
	}
	return id, nil
}

func (m *MockDockerClient) StartContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("StartContainer:" + id); err != nil {
		return err
	}
	c, ok := m.Containers[id]
	if !ok {
		return errors.New("not found")
	}
	c.State = "running"
	m.Containers[id] = c
	return nil
}

func (m *MockDockerClient) StopContainer(_ context.Context, id string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("StopContainer:" + id); err != nil {
		return err
	}
	c, ok := m.Containers[id]
	if !ok {
		return errors.New("not found")
	}
	c.State = "exited"
	m.Containers[id] = c
	return nil
}

func (m *MockDockerClient) RemoveContainer(_ context.Context, id string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("RemoveContainer:" + id); err != nil {
		return err
	}
	delete(m.Containers, id)
	return nil
}

func (m *MockDockerClient) InspectContainer(_ context.Context, id string) (ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("InspectContainer:" + id); err != nil {
		return ContainerInfo{}, err
	}
	c, ok := m.Containers[id]
	if !ok {
		return ContainerInfo{}, errors.New("not found")
	}
	return c, nil
}

func (m *MockDockerClient) ListContainers(_ context.Context, labelFilter map[string]string) ([]ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("ListContainers"); err != nil {
		return nil, err
	}
	var out []ContainerInfo
	for _, c := range m.Containers {
		match := true
		for k, v := range labelFilter {
			if c.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *MockDockerClient) Exec(_ context.Context, id string, _ []string) (io.Reader, io.Reader, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("Exec:" + id); err != nil {
		return nil, nil, 0, err
	}
	stdout := m.ExecOutput[id]
	exit := m.ExecExit[id]
	return strings.NewReader(stdout), strings.NewReader(""), exit, nil
}

func (m *MockDockerClient) RemoveVolume(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.take("RemoveVolume:" + name); err != nil {
		return err
	}
	delete(m.Volumes, name)
	return nil
}

func (m *MockDockerClient) PullImage(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.take("PullImage:" + ref)
}

// SetExecOutputForRole sets ExecOutput for all containers with the given role label.
// Useful for e2e tests where container IDs are not known in advance.
func (m *MockDockerClient) SetExecOutputForRole(role, stdout string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.Containers {
		if c.Labels[LabelRole] == role {
			m.ExecOutput[id] = stdout
		}
	}
}

var _ DockerClient = (*MockDockerClient)(nil)
