package gpuinfo

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/paul/flexctl/internal/flexctlagent"
)

// NvidiaDetector implements flexctlagent.GPUDetector by shelling out to nvidia-smi.
type NvidiaDetector struct {
	lookPath func(string) (string, error)
	timeout  time.Duration
}

type Option func(*NvidiaDetector)

func WithExecLookPath(f func(string) (string, error)) Option {
	return func(d *NvidiaDetector) { d.lookPath = f }
}

func WithTimeout(d time.Duration) Option {
	return func(n *NvidiaDetector) { n.timeout = d }
}

func NewNvidiaDetector(opts ...Option) *NvidiaDetector {
	d := &NvidiaDetector{
		lookPath: exec.LookPath,
		timeout:  3 * time.Second,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Detect returns the GPU inventory. If nvidia-smi is missing or fails,
// returns an empty slice (not an error) so the agent can run on dev hosts
// without GPUs.
func (d *NvidiaDetector) Detect(ctx context.Context) []flexctlagent.GPU {
	bin, err := d.lookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--query-gpu=index,name,memory.total",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("nvidia-smi failed", "err", err)
		return nil
	}
	parsed, err := ParseNvidiaSMI(string(out))
	if err != nil {
		slog.Warn("parse nvidia-smi", "err", err)
		return nil
	}
	return parsed
}

// ParseNvidiaSMI parses the CSV output of `nvidia-smi --query-gpu=index,name,memory.total
// --format=csv,noheader,nounits`. Each line is "INDEX, MODEL, VRAM_MB".
// Whitespace around fields is trimmed.
func ParseNvidiaSMI(out string) ([]flexctlagent.GPU, error) {
	var result []flexctlagent.GPU
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("expected 3 fields per line, got %d in %q", len(parts), line)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("parse index: %w", err)
		}
		model := strings.TrimSpace(parts[1])
		vramStr := strings.TrimSpace(parts[2])
		// Strip optional "MiB" suffix in case --nounits was not honored.
		vramStr = strings.TrimSuffix(vramStr, " MiB")
		vramStr = strings.TrimSuffix(vramStr, "MiB")
		vram, err := strconv.Atoi(strings.TrimSpace(vramStr))
		if err != nil {
			return nil, fmt.Errorf("parse vram: %w", err)
		}
		result = append(result, flexctlagent.GPU{
			Index:  int32(idx),
			Model:  model,
			VRAMMB: int32(vram),
		})
	}
	return result, scanner.Err()
}
