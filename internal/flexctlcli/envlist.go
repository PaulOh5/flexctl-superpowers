package flexctlcli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func NewEnvCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "env", Short: "Inspect environments"}
	cmd.AddCommand(newEnvListCmd())
	return cmd
}

func newEnvListCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "list", Short: "List your environments",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return err }
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			envs, err := c.ListEnvs(cmd.Context())
			if err != nil { return err }
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tHOSTNAME\tSTATUS\tNODE")
			for _, e := range envs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, e.Hostname, e.Status, e.NodeID)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}
