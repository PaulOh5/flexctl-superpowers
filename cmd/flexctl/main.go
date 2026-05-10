package main

import (
	"os"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func main() {
	if err := flexctlcli.Execute(); err != nil {
		os.Exit(1)
	}
}
