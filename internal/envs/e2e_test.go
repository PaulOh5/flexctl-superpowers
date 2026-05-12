package envs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/flexctlagent"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

// startHeadscaleForTest spins up a Headscale 0.23.0 testcontainer using
// config/headscale-test.yaml, creates the `control-plane` user, and mints an
// admin API key. Mirrors internal/headscale/testhelpers_test.go.
func startHeadscaleForTest(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	cfgPath, err := filepath.Abs("../../config/headscale-test.yaml")
	require.NoError(t, err)

	req := testcontainers.ContainerRequest{
		Image:        "headscale/headscale:0.23.0",
		ExposedPorts: []string{"8080/tcp"},
		Cmd:          []string{"serve"},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: cfgPath, ContainerFilePath: "/etc/headscale/config.yaml", FileMode: 0o644},
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
	baseURL := "http://" + host + ":" + port.Port()

	_, _, err = c.Exec(ctx, []string{"headscale", "users", "create", "control-plane"})
	require.NoError(t, err)
	rc, reader, err := c.Exec(ctx, []string{"headscale", "apikeys", "create", "--expiration", "1h"})
	require.NoError(t, err)
	require.Equal(t, 0, rc)
	out := readAllExec(reader)
	apiKey := extractAPIKeyToken(out)
	require.NotEmpty(t, apiKey)
	return baseURL, apiKey
}

// readAllExec demultiplexes Docker exec stream (8-byte header per frame).
func readAllExec(r io.Reader) string {
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

func extractAPIKeyToken(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		token := longestTokenRunE2E(l)
		if len(token) >= 40 {
			return token
		}
	}
	return ""
}

func longestTokenRunE2E(s string) string {
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

// TestE2E_CreateEnvReachesRunning verifies the full flow from HTTP POST /v1/envs
// to status=running via real gRPC + mock Docker.
func TestE2E_CreateEnvReachesRunning(t *testing.T) {
	pool := newPool(t)
	usersSvc := users.NewService(pool)
	nodesSvc := nodes.NewService(pool)
	envsSvc := envs.NewService(pool)
	keysSvc := sshkeys.NewService(pool)

	u, err := usersSvc.Signup(context.Background(), "e@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	_, err = keysSvc.Add(context.Background(), u.ID, "test",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBM5dWmqyhEfP9C1ZDjmh+e9zYx7DbT6JqnNK7NqQy11 paul@laptop")
	require.NoError(t, err)
	pairTok, _ := nodesSvc.CreatePairToken(context.Background(), u.ID)
	node, nodeToken, err := nodesSvc.PairNode(context.Background(), nodes.PairRequest{
		Token: pairTok, Name: "rtx",
	})
	require.NoError(t, err)

	hsURL, hsAPIKey := startHeadscaleForTest(t)
	hsClient := headscale.NewClient(hsURL, hsAPIKey, 5*time.Second)
	_, err = hsClient.CreateUser(context.Background(), "paul")
	require.NoError(t, err)

	agentSrv := agentstream.NewServer(nodesSvc, envsSvc, usersSvc, keysSvc, hsClient)
	envDispatcher := agentstream.NewEnvsDispatcher(agentSrv, hsURL, "flex/sidecar:dev")
	envsH := envs.NewHandlers(envsSvc, envDispatcher)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcSrv, agentSrv)
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		envsH.Mount(r)
	})
	httpSrv := httptest.NewServer(r)
	defer httpSrv.Close()

	mockDocker := envdocker.NewMockDockerClient()
	alloc := envlifecycle.NewGPUAllocator([]int{0, 1})

	agentCfg := flexctlagent.Config{
		GRPCAddress:       lis.Addr().String(),
		NodeToken:         nodeToken,
		AgentVersion:      "0.1.0-test",
		HeartbeatInterval: 100 * time.Millisecond,
		DockerClient:      mockDocker,
		GPUAllocator:      alloc,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := flexctlagent.New(agentCfg)
	go func() { _ = agent.Run(ctx) }()

	// agent 연결 대기
	time.Sleep(500 * time.Millisecond)

	// Mock docker가 어떤 env_id로 만들어진 사이드카에 대해서도 health 응답을 주도록
	// 백그라운드 watcher 실행
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()
	go func() {
		for {
			select {
			case <-watcherCtx.Done():
				return
			case <-time.After(50 * time.Millisecond):
				mockDocker.SetExecOutputForRole("sidecar", "tailscale 100.64.0.5\n")
			}
		}
	}()

	tok, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id":     node.ID.String(),
		"template_id": "cuda-base",
		"name":        "e2etest",
		"gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// DB 폴링 — status=running이 될 때까지
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := envsSvc.ListByOwner(context.Background(), u.ID)
		if len(got) == 1 && got[0].Status == "running" {
			require.NotEmpty(t, got[0].SidecarContainerID)
			require.NotEmpty(t, got[0].DevContainerID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, _ := envsSvc.ListByOwner(context.Background(), u.ID)
	if len(got) > 0 {
		t.Fatalf("env never reached running; status=%s message=%s", got[0].Status, got[0].StatusMessage)
	}
	t.Fatal("env not created")
}

// Ensure grpc/credentials/insecure is used (import kept for clarity).
var _ = insecure.NewCredentials
