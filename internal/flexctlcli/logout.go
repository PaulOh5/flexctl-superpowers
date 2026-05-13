package flexctlcli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func NewLogoutCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "logout", Short: "Delete this device, drop session, clean local state",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath = cfgPathOrDefault(cfgPath)
			cfg, err := ReadClientConfig(cfgPath)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "already logged out")
					return nil
				}
				return err
			}
			api := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)

			// Best-effort: find device by hostname and delete.
			devs, _ := api.ListDevices(cmd.Context())
			for _, d := range devs {
				if d.Hostname == cfg.DeviceHostname {
					_ = api.DeleteDevice(cmd.Context(), d.ID)
				}
			}
			_ = api.Logout(cmd.Context())

			// state dir
			stateDir := DefaultStateDir()
			if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" {
				stateDir = v
			}
			_ = os.RemoveAll(stateDir)

			// ssh config
			sshCfg := os.Getenv("FLEXCTL_SSH_CONFIG")
			if sshCfg == "" {
				sshCfg = DefaultSSHConfigPath()
			}
			_ = RemoveSSHConfigBlock(sshCfg)

			// client.toml
			_ = os.Remove(cfgPath)

			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}
