package flexctlcli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sshBlockBegin = "# >>> flexctl >>>"
	sshBlockEnd   = "# <<< flexctl <<<"
)

type SSHConfigBlock struct {
	FlexctlBinary string // absolute path to flexctl
	KnownHosts    string // path to ~/.config/flexctl/known_hosts
}

func renderBlock(b SSHConfigBlock) string {
	return fmt.Sprintf(`%s
# Managed by flexctl login. Do not edit between markers; changes will be overwritten.
Host *.flex
    User dev
    ProxyCommand %s proxy %%h %%p
    ServerAliveInterval 30
    ServerAliveCountMax 3
    ConnectTimeout 30
    StrictHostKeyChecking accept-new
    UserKnownHostsFile %s
%s
`, sshBlockBegin, b.FlexctlBinary, b.KnownHosts, sshBlockEnd)
}

// UpsertSSHConfig inserts or replaces the flexctl-managed block in `path`.
// Existing content outside the markers is preserved. Backs up the original
// file (if non-empty) the first time the block is inserted.
func UpsertSSHConfig(path string, b SSHConfigBlock) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read: %w", err)
	}

	hadBlock := bytes.Contains(existing, []byte(sshBlockBegin))
	if len(existing) > 0 && !hadBlock {
		bak := fmt.Sprintf("%s.flexctl-bak.%d", path, time.Now().Unix())
		if err := os.WriteFile(bak, existing, 0o600); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}

	newBlock := renderBlock(b)
	var out []byte
	if hadBlock {
		out = replaceBlock(existing, newBlock)
	} else {
		out = appendBlock(existing, newBlock)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return os.WriteFile(path, out, 0o600)
}

// RemoveSSHConfigBlock removes the flexctl-managed marker block from `path`.
// If the file does not exist or has no block, it is a no-op.
func RemoveSSHConfigBlock(path string) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Contains(existing, []byte(sshBlockBegin)) {
		return nil
	}
	bak := fmt.Sprintf("%s.flexctl-bak.%d", path, time.Now().Unix())
	if err := os.WriteFile(bak, existing, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	out := replaceBlock(existing, "")
	return os.WriteFile(path, out, 0o600)
}

func appendBlock(existing []byte, block string) []byte {
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	if len(existing) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteString(block)
	return buf.Bytes()
}

func replaceBlock(existing []byte, replacement string) []byte {
	beginIdx := bytes.Index(existing, []byte(sshBlockBegin))
	if beginIdx < 0 {
		return existing
	}
	endIdx := bytes.Index(existing[beginIdx:], []byte(sshBlockEnd))
	if endIdx < 0 {
		return existing
	}
	endIdx += beginIdx + len(sshBlockEnd)
	if endIdx < len(existing) && existing[endIdx] == '\n' {
		endIdx++
	}

	var buf bytes.Buffer
	buf.Write(existing[:beginIdx])
	if replacement != "" {
		buf.WriteString(replacement)
	} else {
		out := buf.Bytes()
		if len(out) > 0 && out[len(out)-1] == '\n' && len(existing[endIdx:]) == 0 {
			buf.Truncate(len(out) - 1)
		}
	}
	buf.Write(existing[endIdx:])
	return []byte(strings.TrimRight(buf.String(), " \t"))
}

// DefaultSSHConfigPath returns the user's default SSH config file path.
func DefaultSSHConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}
