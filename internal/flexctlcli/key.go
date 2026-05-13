package flexctlcli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func NewKeyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "key", Short: "Manage SSH public keys on the control plane"}
	cmd.AddCommand(newKeyAddCmd())
	cmd.AddCommand(newKeyListCmd())
	cmd.AddCommand(newKeyRmCmd())
	return cmd
}

func newKeyAddCmd() *cobra.Command {
	var (
		name    string
		cfgPath string
	)
	cmd := &cobra.Command{
		Use:   "add <path|->",
		Short: "Upload an SSH public key (path or '-' for stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return fmt.Errorf("read config: %w (try `flexctl login`)", err) }

			var data []byte
			if args[0] == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil { return fmt.Errorf("read key: %w", err) }
			pub := strings.TrimSpace(string(data))

			if name == "" {
				h, _ := os.Hostname()
				name = h
			}
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			id, err := c.AddSSHKey(cmd.Context(), name, pub)
			if err != nil { return err }
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Key name (defaults to hostname)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func newKeyListCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "list", Short: "List uploaded SSH keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return err }
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			keys, err := c.ListSSHKeys(cmd.Context())
			if err != nil { return err }
			for _, k := range keys {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", k.ID, k.Name, k.Fingerprint)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func newKeyRmCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "rm <key-id>",
		Short: "Delete an SSH key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return err }
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			return c.DeleteSSHKey(cmd.Context(), args[0])
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func cfgPathOrDefault(p string) string {
	if p != "" { return p }
	return DefaultClientConfigPath()
}
