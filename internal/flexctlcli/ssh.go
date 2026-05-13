package flexctlcli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// BuildSSHArgs returns the argv for the `ssh` invocation that flexctl ssh
// would exec. Exposed for tests; production code wraps it in NewSSHCmd.
func BuildSSHArgs(ctx context.Context, cfgPath, selfPath, envName string, extra []string) ([]string, error) {
	cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	api := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
	envs, err := api.ListEnvs(ctx)
	if err != nil {
		return nil, err
	}
	var target Env
	for _, e := range envs {
		if e.Name == envName {
			target = e
			break
		}
	}
	if target.ID == "" {
		return nil, fmt.Errorf("env %q not found", envName)
	}
	if target.Status != "running" {
		return nil, fmt.Errorf("env %q is %s (must be running)", envName, target.Status)
	}

	args := []string{
		"-o", "ProxyCommand=" + selfPath + " proxy %h %p",
		"-o", "UserKnownHostsFile=" + DefaultKnownHostsPath(),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=30",
		"dev@" + target.Hostname + ".flex",
	}
	args = append(args, extra...)
	return args, nil
}

func NewSSHCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:                  "ssh <env-name> [-- <ssh-args>]",
		Short:                "SSH into an environment",
		Args:                 cobra.MinimumNArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			selfPath, _ := os.Executable()
			if abs, err := filepath.Abs(selfPath); err == nil {
				selfPath = abs
			}

			envName := args[0]
			extra := args[1:]
			built, err := BuildSSHArgs(cmd.Context(), cfgPath, selfPath, envName, extra)
			if err != nil {
				return err
			}

			c := exec.CommandContext(cmd.Context(), "ssh", built...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}
