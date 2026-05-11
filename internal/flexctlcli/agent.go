package flexctlcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paul/flexctl/internal/flexctlagent"
	"github.com/paul/flexctl/internal/gpuinfo"
)

func NewAgentCmd() *cobra.Command {
	var (
		configPath string
		heartbeat  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the flexctl node agent (long-lived gRPC stream to control plane)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := readAgentConfig(configPath)
			if err != nil {
				return err
			}
			if cfg.GRPCAddress == "" {
				return errors.New("config has empty grpc_address; re-run `flexctl join` with --grpc-address")
			}
			a := flexctlagent.New(flexctlagent.Config{
				GRPCAddress:       cfg.GRPCAddress,
				NodeToken:         cfg.NodeToken,
				AgentVersion:      Version,
				HeartbeatInterval: heartbeat,
				GPUDetector:       gpuinfo.NewNvidiaDetector(),
			})
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return a.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath(), "Path to agent.toml")
	cmd.Flags().DurationVar(&heartbeat, "heartbeat", 30*time.Second, "Heartbeat interval")
	return cmd
}

// WriteAgentConfigForTest exposes config-write for test harnesses.
func WriteAgentConfigForTest(path string, cfg AgentConfig) error {
	return writeAgentConfig(path, cfg)
}

// readAgentConfig reads the TOML file written by `flexctl join`. Hand-parsed
// because we don't want to pull a TOML dep just for 4 keys.
func readAgentConfig(path string) (AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := AgentConfig{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"`)
		switch key {
		case "node_id":
			cfg.NodeID = val
		case "node_token":
			cfg.NodeToken = val
		case "control_plane":
			cfg.ControlPlane = val
		case "grpc_address":
			cfg.GRPCAddress = val
		}
	}
	if cfg.NodeToken == "" {
		return AgentConfig{}, errors.New("config missing node_token")
	}
	return cfg, nil
}
