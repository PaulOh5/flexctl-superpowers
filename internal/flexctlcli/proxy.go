package flexctlcli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"tailscale.com/tsnet"

	"github.com/paul/flexctl/internal/flextsnet"
)

// NormalizeProxyHost strips the trailing ".flex" suffix that ssh_config adds.
func NormalizeProxyHost(h string) string { return strings.TrimSuffix(h, ".flex") }

func NewProxyCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "proxy <host> <port>",
		Short: "OpenSSH ProxyCommand entry point (pipes stdin/stdout via tsnet)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil {
				return fmt.Errorf("read config: %w (run `flexctl login`)", err)
			}

			host := NormalizeProxyHost(args[0])
			port := args[1]

			ctx := cmd.Context()
			srv, err := startWithRetry(ctx, cfg)
			if err != nil {
				return err
			}
			defer srv.Close()

			conn, err := flextsnet.Dial(ctx, srv, host, port)
			if err != nil {
				return fmt.Errorf("dial %s:%s: %w", host, port, err)
			}
			return flextsnet.Pipe(conn, os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func startWithRetry(ctx context.Context, cfg ClientConfig) (*tsnet.Server, error) {
	stateDir := DefaultStateDir()
	if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" {
		stateDir = v
	}
	for attempt := 0; attempt < 2; attempt++ {
		srv, err := flextsnet.Start(ctx, flextsnet.Config{
			StateDir:   stateDir,
			Hostname:   cfg.DeviceHostname,
			ControlURL: cfg.HeadscaleURL,
		})
		if err == nil {
			return srv, nil
		}
		if attempt == 0 {
			time.Sleep(5 * time.Second)
			continue
		}
		return nil, fmt.Errorf("tsnet start (another flexctl proxy may be initializing): %w", err)
	}
	return nil, nil
}
