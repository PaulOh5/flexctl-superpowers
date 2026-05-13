package flexctlcli

import (
	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "flexctl",
		Short:         "flexctl — self-sovereign GPU cloud agent + client",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(NewJoinCmd())
	root.AddCommand(NewAgentCmd())
	root.AddCommand(NewSidecarCmd())
	root.AddCommand(NewKeyCmd())
	root.AddCommand(NewEnvCmd())
	return root
}

func Execute() error {
	return NewRootCmd().Execute()
}
