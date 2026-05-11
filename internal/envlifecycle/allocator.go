package envlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/envdocker"
)

var ErrInsufficientGPUs = errors.New("insufficient GPUs")

type GPUAllocator struct {
	mu        sync.Mutex
	total     []int
	allocated map[uuid.UUID][]int
}

func NewGPUAllocator(totalIndices []int) *GPUAllocator {
	cp := append([]int(nil), totalIndices...)
	sort.Ints(cp)
	return &GPUAllocator{total: cp, allocated: map[uuid.UUID][]int{}}
}

// Allocate reserves `count` indices for envID. Returns the chosen indices (sorted).
// count=0 returns an empty slice and reserves nothing.
func (a *GPUAllocator) Allocate(envID uuid.UUID, count int) ([]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if count == 0 {
		a.allocated[envID] = nil
		return []int{}, nil
	}
	used := map[int]bool{}
	for _, idxs := range a.allocated {
		for _, i := range idxs {
			used[i] = true
		}
	}
	var free []int
	for _, i := range a.total {
		if !used[i] {
			free = append(free, i)
		}
	}
	if len(free) < count {
		return nil, fmt.Errorf("%w: requested %d, available %d", ErrInsufficientGPUs, count, len(free))
	}
	pick := append([]int(nil), free[:count]...)
	a.allocated[envID] = pick
	return pick, nil
}

func (a *GPUAllocator) Release(envID uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocated, envID)
}

// RestoreFromDocker walks the dev containers and rebuilds the allocation map
// from flexctl.gpu_indices labels. Called once at agent startup.
func (a *GPUAllocator) RestoreFromDocker(ctx context.Context, c envdocker.DockerClient) error {
	list, err := c.ListContainers(ctx, map[string]string{
		envdocker.LabelRole: "dev",
	})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, info := range list {
		envIDStr := info.Labels[envdocker.LabelEnvID]
		if envIDStr == "" {
			continue
		}
		envID, err := uuid.Parse(envIDStr)
		if err != nil {
			continue
		}
		idxStr := info.Labels[envdocker.LabelGPUIndices]
		indices := parseIndicesCSV(idxStr)
		a.allocated[envID] = indices
	}
	return nil
}

// Restored returns the allocation map (test helper).
func (a *GPUAllocator) Restored() map[uuid.UUID][]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[uuid.UUID][]int, len(a.allocated))
	for k, v := range a.allocated {
		out[k] = append([]int(nil), v...)
	}
	return out
}

func parseIndicesCSV(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// IndicesCSV formats indices for a container label.
func IndicesCSV(idx []int) string {
	parts := make([]string, 0, len(idx))
	for _, i := range idx {
		parts = append(parts, strconv.Itoa(i))
	}
	return strings.Join(parts, ",")
}
