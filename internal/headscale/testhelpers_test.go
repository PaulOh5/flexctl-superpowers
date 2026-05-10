package headscale_test

import (
	"context"
	"encoding/binary"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startHeadscale boots Headscale 0.23.0 with sqlite backend, mounts the project's
// config/headscale.yaml, exec's `headscale apikeys create` to mint an API token,
// and returns (baseURL, apiKey).
func startHeadscale(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	cfgPath, err := filepath.Abs("../../config/headscale-test.yaml")
	require.NoError(t, err)

	req := testcontainers.ContainerRequest{
		Image:        "headscale/headscale:0.23.0",
		ExposedPorts: []string{"8080/tcp"},
		Cmd:          []string{"serve"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      cfgPath,
				ContainerFilePath: "/etc/headscale/config.yaml",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForHTTP("/health").
			WithPort("8080/tcp").
			WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)

	// Mint API key via exec
	_, _, err = c.Exec(ctx, []string{"headscale", "users", "create", "control-plane"})
	require.NoError(t, err)
	rc, reader, err := c.Exec(ctx, []string{"headscale", "apikeys", "create", "--expiration", "1h"})
	require.NoError(t, err)
	require.Equal(t, 0, rc)
	out := readAll(t, reader)
	apiKey := extractAPIKey(t, out)

	baseURL := "http://" + host + ":" + port.Port()
	return baseURL, apiKey
}

// readAll reads the Docker exec multiplexed stream and returns the demultiplexed
// stdout content. Docker exec streams data with 8-byte headers:
// [stream_type(1), 0, 0, 0, size_big_endian(4)] then `size` bytes of content.
// stream_type 1 = stdout, 2 = stderr; we collect both.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	var buf []byte
	header := make([]byte, 8)
	for {
		_, err := io.ReadFull(r, header)
		if err != nil {
			break
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		data := make([]byte, size)
		_, err = io.ReadFull(r, data)
		if err != nil {
			buf = append(buf, data...)
			break
		}
		buf = append(buf, data...)
	}
	return string(buf)
}

func extractAPIKey(t *testing.T, out string) string {
	t.Helper()
	// `headscale apikeys create` prints noise like timestamps then the key on the last non-empty line.
	// Some version prefix the line with control characters (multiplexed docker exec stream).
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		// Strip docker exec stream control bytes if present (first 8 bytes are header).
		// Heuristic: token is alphanumeric + underscore/dot/dash, length ~40-80.
		// Take the longest contiguous run of [A-Za-z0-9._-] >= 40 chars.
		token := longestTokenRun(l)
		if len(token) >= 40 {
			return token
		}
	}
	t.Fatalf("could not extract API key from output: %q", out)
	return ""
}

func longestTokenRun(s string) string {
	var best, cur strings.Builder
	bestLen := 0
	for _, ch := range s {
		isTok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '-'
		if isTok {
			cur.WriteRune(ch)
			if cur.Len() > bestLen {
				bestLen = cur.Len()
				best.Reset()
				best.WriteString(cur.String())
			}
		} else {
			cur.Reset()
		}
	}
	return best.String()
}
