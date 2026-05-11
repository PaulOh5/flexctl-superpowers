package flexctlcli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func NewSidecarCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "sidecar",
		Short:  "Run inside the sidecar container: bring up tailscaled + tailscale up",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hostname := os.Getenv("FLEXCTL_HOSTNAME")
			authkey := os.Getenv("FLEXCTL_AUTHKEY")
			hsURL := os.Getenv("FLEXCTL_HEADSCALE_URL")
			tags := os.Getenv("FLEXCTL_TAGS")
			if hostname == "" || authkey == "" || hsURL == "" {
				return errors.New("FLEXCTL_HOSTNAME, FLEXCTL_AUTHKEY, FLEXCTL_HEADSCALE_URL required")
			}

			if err := os.MkdirAll("/var/lib/tailscale", 0o755); err != nil {
				return fmt.Errorf("mkdir state: %w", err)
			}
			if err := os.MkdirAll("/var/run/tailscale", 0o755); err != nil {
				return fmt.Errorf("mkdir run: %w", err)
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			daemon := exec.CommandContext(ctx, "tailscaled",
				"--state=/var/lib/tailscale/tailscaled.state",
				"--socket=/var/run/tailscale/tailscaled.sock",
				"--tun=tailscale0")
			daemon.Stdout = os.Stdout
			daemon.Stderr = os.Stderr
			if err := daemon.Start(); err != nil {
				return fmt.Errorf("start tailscaled: %w", err)
			}
			slog.Info("tailscaled started", "pid", daemon.Process.Pid)

			// Wait briefly for the daemon socket
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat("/var/run/tailscale/tailscaled.sock"); err == nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}

			upArgs := []string{
				"--login-server=" + hsURL,
				"--authkey=" + authkey,
				"--hostname=" + hostname,
				"--accept-dns=false",
			}
			if t := strings.TrimSpace(tags); t != "" {
				upArgs = append(upArgs, "--advertise-tags="+t)
			}
			up := exec.CommandContext(ctx, "tailscale", append([]string{"up"}, upArgs...)...)
			up.Stdout = os.Stdout
			up.Stderr = os.Stderr
			if err := up.Run(); err != nil {
				_ = daemon.Process.Signal(syscall.SIGTERM)
				return fmt.Errorf("tailscale up: %w", err)
			}
			slog.Info("tailscale up succeeded", "hostname", hostname)

			// Wait for SIGTERM, then logout + stop daemon.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			<-sigCh
			slog.Info("sidecar shutting down")
			_ = exec.Command("tailscale", "logout").Run()
			_ = daemon.Process.Signal(syscall.SIGTERM)
			_ = daemon.Wait()
			return nil
		},
	}
}
