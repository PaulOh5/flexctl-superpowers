package flexctlcli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ClientConfig struct {
	ControlPlane   string
	SessionCookie  string
	DeviceName     string
	DeviceHostname string
	HeadscaleURL   string
	TailnetDomain  string
}

func WriteClientConfig(path string, c ClientConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	contents := fmt.Sprintf(`control_plane = %q
session_cookie = %q
device_name = %q
device_hostname = %q
headscale_url = %q
tailnet_domain = %q
`, c.ControlPlane, c.SessionCookie, c.DeviceName,
		c.DeviceHostname, c.HeadscaleURL, c.TailnetDomain)
	return os.WriteFile(path, []byte(contents), 0o600)
}

func ReadClientConfig(path string) (ClientConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return ClientConfig{}, err
	}
	defer f.Close()

	out := ClientConfig{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		raw := strings.TrimSpace(line[eq+1:])
		v, err := strconv.Unquote(raw)
		if err != nil {
			continue
		}
		switch k {
		case "control_plane":
			out.ControlPlane = v
		case "session_cookie":
			out.SessionCookie = v
		case "device_name":
			out.DeviceName = v
		case "device_hostname":
			out.DeviceHostname = v
		case "headscale_url":
			out.HeadscaleURL = v
		case "tailnet_domain":
			out.TailnetDomain = v
		}
	}
	return out, sc.Err()
}

// DefaultClientConfigPath returns ~/.config/flexctl/client.toml, honoring $FLEXCTL_CLIENT_CONFIG.
func DefaultClientConfigPath() string {
	if v := os.Getenv("FLEXCTL_CLIENT_CONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "flexctl", "client.toml")
}

// DefaultStateDir returns ~/.config/flexctl/tsnet/, honoring $FLEXCTL_STATE_DIR.
func DefaultStateDir() string {
	if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "flexctl", "tsnet")
}

// DefaultKnownHostsPath returns ~/.config/flexctl/known_hosts.
func DefaultKnownHostsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "flexctl", "known_hosts")
}
