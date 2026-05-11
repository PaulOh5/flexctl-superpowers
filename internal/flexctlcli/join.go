package flexctlcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// AgentConfig is what `flexctl join` writes to disk for `flexctl agent` to read.
type AgentConfig struct {
	NodeID       string `toml:"node_id"`
	NodeToken    string `toml:"node_token"`
	ControlPlane string `toml:"control_plane"`
	GRPCAddress  string `toml:"grpc_address"`
}

func NewJoinCmd() *cobra.Command {
	var (
		name         string
		controlPlane string
		grpcAddr     string
		configPath   string
	)
	cmd := &cobra.Command{
		Use:   "join <pair-token>",
		Short: "Pair this node with the control plane and persist the config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := args[0]
			if name == "" {
				h, _ := os.Hostname()
				name = h
			}
			if name == "" {
				return fmt.Errorf("--name is required (and hostname could not be detected)")
			}
			if controlPlane == "" {
				return fmt.Errorf("--control-plane is required")
			}
			body, _ := json.Marshal(map[string]any{
				"token":    token,
				"name":     name,
				"gpu_info": []any{},
			})
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost,
				controlPlane+"/v1/nodes/pair", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")

			httpClient := &http.Client{Timeout: 30 * time.Second}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("call control-plane: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("pair failed: %s", resp.Status)
			}
			var pairResp struct {
				NodeID    string `json:"node_id"`
				NodeToken string `json:"node_token"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&pairResp); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			cfg := AgentConfig{
				NodeID:       pairResp.NodeID,
				NodeToken:    pairResp.NodeToken,
				ControlPlane: controlPlane,
				GRPCAddress:  grpcAddr,
			}
			if err := writeAgentConfig(configPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Paired as %s (node_id=%s)\nConfig: %s\n",
				name, pairResp.NodeID, configPath)
			fmt.Fprintln(cmd.OutOrStdout(), "Next: run `flexctl agent` (and see README for systemd unit).")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Node name (defaults to hostname)")
	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "Control plane HTTP URL (e.g. https://flexctl.example.com)")
	cmd.Flags().StringVar(&grpcAddr, "grpc-address", "", "gRPC agent stream address, e.g. flexctl.example.com:9090 (required)")
	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath(), "Path to write agent config")
	return cmd
}

func writeAgentConfig(path string, cfg AgentConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	contents := fmt.Sprintf(`node_id = %q
node_token = %q
control_plane = %q
grpc_address = %q
`, cfg.NodeID, cfg.NodeToken, cfg.ControlPlane, cfg.GRPCAddress)
	return os.WriteFile(path, []byte(contents), 0o600)
}

func defaultConfigPath() string {
	if v := os.Getenv("FLEXCTL_CONFIG"); v != "" {
		return v
	}
	return "/etc/flexctl/agent.toml"
}
