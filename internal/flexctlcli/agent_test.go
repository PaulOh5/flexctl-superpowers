package flexctlcli_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/grpc"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/flexctlcli"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/users"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("flex"),
		tcpostgres.WithUsername("flex"),
		tcpostgres.WithPassword("flex"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE TABLE users (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			email text NOT NULL UNIQUE,
			slug text NOT NULL UNIQUE,
			password_hash text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE pair_tokens (
			token_hash text PRIMARY KEY,
			user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at timestamptz NOT NULL,
			used_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE nodes (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name text NOT NULL,
			agent_version text NOT NULL DEFAULT '',
			gpu_info jsonb NOT NULL DEFAULT '[]'::jsonb,
			status text NOT NULL DEFAULT 'offline',
			node_token_hash text NOT NULL UNIQUE,
			last_seen_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now(),
			UNIQUE(owner_user_id, name)
		);
	`)
	require.NoError(t, err)
	return pool
}

// TestAgentCommand_E2E pairs a node, runs `flexctl agent`, and verifies the
// node row reaches status=online with non-empty agent_version.
func TestAgentCommand_E2E(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)

	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	pairToken, err := svc.CreatePairToken(context.Background(), u.ID)
	require.NoError(t, err)
	node, nodeToken, err := svc.PairNode(context.Background(), nodes.PairRequest{
		Token: pairToken, Name: "rtx",
	})
	require.NoError(t, err)

	// Boot gRPC server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcSrv, agentstream.NewServer(svc))
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	// Write agent config
	cfgPath := filepath.Join(t.TempDir(), "agent.toml")
	require.NoError(t, flexctlcli.WriteAgentConfigForTest(cfgPath, flexctlcli.AgentConfig{
		NodeID:       node.ID.String(),
		NodeToken:    nodeToken,
		ControlPlane: "http://unused-in-this-test",
		GRPCAddress:  lis.Addr().String(),
	}))

	// Run `flexctl agent --config <path>` in a goroutine; cancel after we verify.
	cmd := flexctlcli.NewAgentCmd()
	cmd.SetArgs([]string{"--config", cfgPath, "--heartbeat", "200ms"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Execute()
	}()

	// Poll DB until status=online (agent registered) or timeout.
	deadline := time.Now().Add(5 * time.Second)
	var got nodes.Node
	for time.Now().Before(deadline) {
		got, err = svc.AuthenticateNodeToken(context.Background(), nodeToken)
		require.NoError(t, err)
		if got.Status == "online" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify DB while agent is still running (stream open → status stays online)
	require.Equal(t, "online", got.Status)
	require.NotEmpty(t, got.AgentVersion)

	// Shut down agent
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not stop within 3s")
	}
}
