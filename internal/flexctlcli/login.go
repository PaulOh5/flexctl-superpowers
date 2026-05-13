package flexctlcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/paul/flexctl/internal/flextsnet"
)

func NewLoginCmd() *cobra.Command {
	var (
		controlPlane string
		email        string
		password     string
		deviceName   string
		cfgPath      string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate, upload SSH keys, pair this device, join the tailnet",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfgPath = cfgPathOrDefault(cfgPath)

			if controlPlane == "" {
				if existing, err := ReadClientConfig(cfgPath); err == nil {
					controlPlane = existing.ControlPlane
				}
			}
			if controlPlane == "" {
				return fmt.Errorf("--control-plane is required on first login")
			}
			if email == "" || password == "" {
				var err error
				email, password, err = promptCreds(email)
				if err != nil {
					return err
				}
			}
			if deviceName == "" {
				h, _ := os.Hostname()
				deviceName = sanitizeDeviceName(h)
			}

			// 1. Session
			api := NewAPIClient(controlPlane, "")
			cookie, err := api.Login(ctx, email, password)
			if err != nil {
				return fmt.Errorf("login: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ session established")

			// 2. Auto key upload (best effort)
			if err := autoUploadKeys(ctx, api); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: key upload: %v\n", err)
			}

			// 3. Device pair
			pair, err := api.PairDevice(ctx, deviceName)
			if err != nil {
				return fmt.Errorf("pair device: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ device paired as %s\n", pair.Hostname)

			// 4. tsnet join (skippable for unit tests)
			if os.Getenv("FLEXCTL_SKIP_TSNET") != "1" {
				stateDir := DefaultStateDir()
				if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" {
					stateDir = v
				}
				if err := os.MkdirAll(stateDir, 0o700); err != nil {
					return fmt.Errorf("state dir: %w", err)
				}
				srv, err := flextsnet.Start(ctx, flextsnet.Config{
					StateDir:   stateDir,
					Hostname:   pair.Hostname,
					AuthKey:    pair.PreauthKey,
					ControlURL: pair.HeadscaleURL,
				})
				if err != nil {
					return fmt.Errorf("tsnet up: %w", err)
				}
				_ = srv.Close()
				fmt.Fprintln(cmd.OutOrStdout(), "✓ joined tailnet")
			}

			// 5. ssh_config block
			selfPath, _ := os.Executable()
			if abs, err := filepath.Abs(selfPath); err == nil {
				selfPath = abs
			}
			sshCfgPath := os.Getenv("FLEXCTL_SSH_CONFIG")
			if sshCfgPath == "" {
				sshCfgPath = DefaultSSHConfigPath()
			}
			if err := UpsertSSHConfig(sshCfgPath, SSHConfigBlock{
				FlexctlBinary: selfPath, KnownHosts: DefaultKnownHostsPath(),
			}); err != nil {
				return fmt.Errorf("ssh config: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ ~/.ssh/config updated")

			// Persist
			cfg := ClientConfig{
				ControlPlane:   controlPlane,
				SessionCookie:  cookie,
				DeviceName:     deviceName,
				DeviceHostname: pair.Hostname,
				HeadscaleURL:   pair.HeadscaleURL,
				TailnetDomain:  pair.TailnetDomain,
			}
			if err := WriteClientConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Done. Try: flexctl env list\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "Control plane URL")
	cmd.Flags().StringVar(&email, "email", "", "Account email (prompted if empty)")
	cmd.Flags().StringVar(&password, "password", "", "Account password (prompted if empty)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "Device name (default OS hostname)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

// promptCreds reads email (if empty) and password from stderr/terminal.
func promptCreds(email string) (string, string, error) {
	if email == "" {
		fmt.Fprint(os.Stderr, "email: ")
		var e string
		_, err := fmt.Fscanln(os.Stdin, &e)
		if err != nil {
			return "", "", err
		}
		email = strings.TrimSpace(e)
	}
	fmt.Fprint(os.Stderr, "password: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", "", err
	}
	return email, string(pw), nil
}

func sanitizeDeviceName(s string) string {
	s = strings.ToLower(s)
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "device"
	}
	return string(out)
}

func autoUploadKeys(ctx context.Context, api *APIClient) error {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".ssh", "id_ed25519.pub"),
		filepath.Join(home, ".ssh", "id_rsa.pub"),
		filepath.Join(home, ".ssh", "id_ecdsa.pub"),
	}
	existing, err := api.ListSSHKeys(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(existing))
	for _, k := range existing {
		known[k.Fingerprint] = true
	}
	osHost, _ := os.Hostname()
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		pub := strings.TrimSpace(string(data))
		fp, err := fingerprintOf(pub)
		if err != nil {
			continue
		}
		if known[fp] {
			continue
		}
		keytype := keytypeFromBasename(filepath.Base(p))
		name := osHost + "-" + keytype
		if _, err := api.AddSSHKey(ctx, name, pub); err != nil {
			return fmt.Errorf("upload %s: %w", p, err)
		}
	}
	return nil
}

func fingerprintOf(pub string) (string, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		return "", err
	}
	// Match Plan 2 internal/sshkeys/service.go (base64 SHA256).
	return ssh.FingerprintSHA256(pk), nil
}

func keytypeFromBasename(name string) string {
	switch {
	case strings.Contains(name, "ed25519"):
		return "ed25519"
	case strings.Contains(name, "ecdsa"):
		return "ecdsa"
	case strings.Contains(name, "rsa"):
		return "rsa"
	default:
		return "key"
	}
}
