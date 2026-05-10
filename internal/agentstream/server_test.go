package agentstream_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
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

// startServer boots a gRPC server on a random port wired to a real Postgres
// testcontainer and returns (gRPC client, nodes.Service, cleanup).
func startServer(t *testing.T) (agentpb.AgentClient, *nodes.Service, func()) {
	t.Helper()
	pool := newTestPool(t)
	svc := nodes.NewService(pool)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	agentpb.RegisterAgentServer(srv, agentstream.NewServer(svc))

	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	cleanup := func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
	return agentpb.NewAgentClient(conn), svc, cleanup
}

func TestStream_RegisterAndHeartbeat(t *testing.T) {
	client, svc, cleanup := startServer(t)
	defer cleanup()

	// Set up a paired node
	ctx := context.Background()
	usersSvc := users.NewService(nodes.PoolFor(svc))
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	pairToken, err := svc.CreatePairToken(ctx, u.ID)
	require.NoError(t, err)
	node, nodeToken, err := svc.PairNode(ctx, nodes.PairRequest{Token: pairToken, Name: "rtx"})
	require.NoError(t, err)

	// Open stream with auth
	md := metadata.Pairs("node-token", nodeToken)
	streamCtx := metadata.NewOutgoingContext(ctx, md)
	stream, err := client.Stream(streamCtx)
	require.NoError(t, err)

	// Send Register
	require.NoError(t, stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{
			Register: &agentpb.Register{
				AgentVersion: "0.1.0",
				Gpus: []*agentpb.GPU{
					{Index: 0, Model: "RTX 4090", VramMb: 24576},
				},
			},
		},
	}))

	// Receive RegisterAck
	resp, err := stream.Recv()
	require.NoError(t, err)
	ack := resp.GetRegisterAck()
	require.NotNil(t, ack)
	require.Equal(t, node.ID.String(), ack.NodeId)

	// Send Heartbeat
	require.NoError(t, stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Heartbeat{
			Heartbeat: &agentpb.Heartbeat{At: timestamppb.Now()},
		},
	}))

	// Receive HeartbeatAck
	resp, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, resp.GetHeartbeatAck())

	// Verify DB state
	gotNode, err := svc.AuthenticateNodeToken(ctx, nodeToken)
	require.NoError(t, err)
	require.Equal(t, "online", gotNode.Status)
	require.Equal(t, "0.1.0", gotNode.AgentVersion)

	// Close stream — server should mark offline
	require.NoError(t, stream.CloseSend())
	time.Sleep(300 * time.Millisecond)
	gotNode, err = svc.AuthenticateNodeToken(ctx, nodeToken)
	require.NoError(t, err)
	require.Equal(t, "offline", gotNode.Status)
}

func TestStream_RejectsMissingToken(t *testing.T) {
	client, _, cleanup := startServer(t)
	defer cleanup()

	stream, err := client.Stream(context.Background())
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err, "stream must reject without node-token metadata")
}

func TestStream_RejectsBadToken(t *testing.T) {
	client, _, cleanup := startServer(t)
	defer cleanup()

	md := metadata.Pairs("node-token", "bogus-token-not-real")
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.Stream(ctx)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err)
}

func TestStream_FirstMessageMustBeRegister(t *testing.T) {
	client, svc, cleanup := startServer(t)
	defer cleanup()

	ctx := context.Background()
	usersSvc := users.NewService(nodes.PoolFor(svc))
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	pairToken, _ := svc.CreatePairToken(ctx, u.ID)
	_, nodeToken, _ := svc.PairNode(ctx, nodes.PairRequest{Token: pairToken, Name: "rtx"})

	md := metadata.Pairs("node-token", nodeToken)
	streamCtx := metadata.NewOutgoingContext(ctx, md)
	stream, err := client.Stream(streamCtx)
	require.NoError(t, err)

	// Send Heartbeat first (out of order)
	require.NoError(t, stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Heartbeat{Heartbeat: &agentpb.Heartbeat{At: timestamppb.Now()}},
	}))
	_, err = stream.Recv()
	require.Error(t, err, "first message must be Register")
}
