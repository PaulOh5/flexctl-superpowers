//go:build integration

package flextsnet_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/paul/flexctl/internal/flextsnet"
	"github.com/paul/flexctl/internal/headscale"
)

// getFreePort returns an available TCP port on localhost by briefly opening a
// listener on :0 and returning the assigned port. The listener is closed before
// returning, so the port is not held; there is a small TOCTOU window.
func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// startHeadscaleForTsnet starts a Headscale 0.23.0 testcontainer and ensures
// that:
//   - container port 8080 is bound to a known host port (chosen with getFreePort),
//   - HEADSCALE_SERVER_URL is overridden to http://localhost:<port> so that
//     tsnet nodes can reach both the coordination API and the embedded DERP
//     relay via the same host port.
//
// Returns (baseURL, apiKey). The container is terminated via t.Cleanup.
func startHeadscaleForTsnet(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	// Determine the config file path relative to this file's location.
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	cfgPath, err := filepath.Abs(filepath.Join(projectRoot, "config", "headscale-test.yaml"))
	require.NoError(t, err)

	port := getFreePort(t)
	hostPort := fmt.Sprintf("%d", port)
	serverURL := fmt.Sprintf("http://localhost:%d", port)

	req := testcontainers.ContainerRequest{
		Image:        "headscale/headscale:0.23.0",
		ExposedPorts: []string{"8080/tcp"},
		Cmd:          []string{"serve"},
		Env: map[string]string{
			// Override server_url so the embedded DERP advertises the same
			// host:port that tsnet uses as ControlURL.
			"HEADSCALE_SERVER_URL": serverURL,
		},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      cfgPath,
				ContainerFilePath: "/etc/headscale/config.yaml",
				FileMode:          0o644,
			},
		},
		HostConfigModifier: func(hc *dockercontainer.HostConfig) {
			// Bind container port 8080 to the chosen host port so that
			// ControlURL and DERP URL both resolve to the same address.
			hc.PortBindings = nat.PortMap{
				"8080/tcp": []nat.PortBinding{
					{HostIP: "127.0.0.1", HostPort: hostPort},
				},
			}
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

	// Create control-plane user and mint an API key.
	_, _, err = c.Exec(ctx, []string{"headscale", "users", "create", "control-plane"})
	require.NoError(t, err)

	rc, reader, err := c.Exec(ctx, []string{"headscale", "apikeys", "create", "--expiration", "1h"})
	require.NoError(t, err)
	require.Equal(t, 0, rc)
	apiKey := tsnetExtractAPIKey(t, tsnetReadDockerStream(reader))
	require.NotEmpty(t, apiKey)

	return serverURL, apiKey
}

// tsnetReadDockerStream demultiplexes the Docker exec multiplexed stream
// (8-byte frame headers).
func tsnetReadDockerStream(r io.Reader) string {
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
		n, err := io.ReadFull(r, data)
		if n > 0 {
			buf = append(buf, data[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

// tsnetExtractAPIKey extracts the API key token from headscale apikeys create
// output. The token is the longest alphanumeric+._- run of >= 40 chars.
func tsnetExtractAPIKey(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		token := tsnetLongestTokenRun(l)
		if len(token) >= 40 {
			return token
		}
	}
	t.Fatalf("could not extract API key from output: %q", out)
	return ""
}

func tsnetLongestTokenRun(s string) string {
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

// TestTsnet_TwoNodesDialEachOther verifies that two tsnet nodes on the same
// Headscale tailnet (same user, same ACL tag) can establish a TCP connection.
func TestTsnet_TwoNodesDialEachOther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	hsURL, apiKey := startHeadscaleForTsnet(t)
	hs := headscale.NewClient(hsURL, apiKey, 10*time.Second)

	_, err := hs.CreateUser(ctx, "paul")
	require.NoError(t, err)

	require.NoError(t, hs.SetPolicy(ctx, `{
  "tagOwners": {"tag:device-paul": ["control-plane"]},
  "acls": [{"action": "accept", "src": ["tag:device-paul"], "dst": ["tag:device-paul:*"]}]
}`))

	keyA, err := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Reusable: false, Ephemeral: false,
		Expiration: 1 * time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	require.NoError(t, err)
	keyB, err := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Reusable: false, Ephemeral: false,
		Expiration: 1 * time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	require.NoError(t, err)

	srvA, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir:   t.TempDir(),
		Hostname:   "paul-device-a",
		AuthKey:    keyA.Key,
		ControlURL: hsURL,
	})
	require.NoError(t, err)
	defer srvA.Close()

	srvB, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir:   t.TempDir(),
		Hostname:   "paul-device-b",
		AuthKey:    keyB.Key,
		ControlURL: hsURL,
	})
	require.NoError(t, err)
	defer srvB.Close()

	ln, err := srvB.Listen("tcp", ":8765")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("hello from B"))
		// Close the connection after writing so the client sees EOF.
	}()

	conn, err := flextsnet.Dial(ctx, srvA, "paul-device-b", "8765")
	require.NoError(t, err)
	defer conn.Close()

	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	require.Equal(t, "hello from B", string(buf[:n]))
}

// TestTsnet_CrossUserACLBlocks verifies that two tsnet nodes in different users
// (and different ACL tag namespaces) cannot dial each other.
func TestTsnet_CrossUserACLBlocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	hsURL, apiKey := startHeadscaleForTsnet(t)
	hs := headscale.NewClient(hsURL, apiKey, 10*time.Second)

	_, err := hs.CreateUser(ctx, "paul")
	require.NoError(t, err)
	_, err = hs.CreateUser(ctx, "alice")
	require.NoError(t, err)

	require.NoError(t, hs.SetPolicy(ctx, `{
  "tagOwners": {
    "tag:device-paul":  ["control-plane"],
    "tag:device-alice": ["control-plane"]
  },
  "acls": [
    {"action": "accept", "src": ["tag:device-paul"],  "dst": ["tag:device-paul:*"]},
    {"action": "accept", "src": ["tag:device-alice"], "dst": ["tag:device-alice:*"]}
  ]
}`))

	keyP, err := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Expiration: time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	require.NoError(t, err)
	keyA, err := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "alice", Expiration: time.Hour, ACLTags: []string{"tag:device-alice"},
	})
	require.NoError(t, err)

	srvP, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir:   t.TempDir(),
		Hostname:   "paul-device-x",
		AuthKey:    keyP.Key,
		ControlURL: hsURL,
	})
	require.NoError(t, err)
	defer srvP.Close()

	srvA, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir:   t.TempDir(),
		Hostname:   "alice-device-y",
		AuthKey:    keyA.Key,
		ControlURL: hsURL,
	})
	require.NoError(t, err)
	defer srvA.Close()

	ln, err := srvA.Listen("tcp", ":9090")
	require.NoError(t, err)
	defer ln.Close()
	go func() { _, _ = ln.Accept() }()

	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	_, err = srvP.Dial(dctx, "tcp", net.JoinHostPort("alice-device-y", "9090"))
	require.Error(t, err, "ACL must block cross-user dial")
}
