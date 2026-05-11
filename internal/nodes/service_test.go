package nodes_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

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

func mkUser(t *testing.T, pool *pgxpool.Pool, email, slug string) uuid.UUID {
	t.Helper()
	svc := users.NewService(pool)
	u, err := svc.Signup(context.Background(), email, slug, "correct-horse-battery")
	require.NoError(t, err)
	return u.ID
}

func TestCreatePairToken_FormatAndExpiry(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	token, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "FX-"), "token must have FX- prefix, got %q", token)
	require.GreaterOrEqual(t, len(token), 60, "token should be reasonably long")
}

func TestPairNode_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	token, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)

	node, nodeToken, err := svc.PairNode(context.Background(), nodes.PairRequest{
		Token:   token,
		Name:    "rtx4090",
		GPUInfo: []byte(`[{"index":0,"model":"NVIDIA RTX 4090","vram_mb":24576}]`),
	})
	require.NoError(t, err)
	require.NotEmpty(t, nodeToken)
	require.Equal(t, "rtx4090", node.Name)
	require.Equal(t, uid, node.OwnerUserID)
}

func TestPairNode_TokenAlreadyUsed(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)

	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "n1"})
	require.NoError(t, err)

	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "n2"})
	require.ErrorIs(t, err, nodes.ErrTokenInvalid)
}

func TestPairNode_TokenExpired(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	// Insert a manually-expired token.
	tokenHash, err := svc.HashTokenForTest("FX-expired-token-please-fail")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO pair_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, uid, time.Now().Add(-time.Minute))
	require.NoError(t, err)

	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{
		Token: "FX-expired-token-please-fail", Name: "n1",
	})
	require.ErrorIs(t, err, nodes.ErrTokenInvalid)
}

func TestPairNode_DuplicateNodeName(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	t1, _ := svc.CreatePairToken(context.Background(), uid)
	_, _, err := svc.PairNode(context.Background(), nodes.PairRequest{Token: t1, Name: "rtx"})
	require.NoError(t, err)

	t2, _ := svc.CreatePairToken(context.Background(), uid)
	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{Token: t2, Name: "rtx"})
	require.ErrorIs(t, err, nodes.ErrNodeNameTaken)
}

func TestAuthenticateNodeToken(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, _ := svc.CreatePairToken(context.Background(), uid)
	node, nodeToken, err := svc.PairNode(context.Background(), nodes.PairRequest{
		Token: token, Name: "rtx",
	})
	require.NoError(t, err)

	got, err := svc.AuthenticateNodeToken(context.Background(), nodeToken)
	require.NoError(t, err)
	require.Equal(t, node.ID, got.ID)

	_, err = svc.AuthenticateNodeToken(context.Background(), "bogus-token")
	require.ErrorIs(t, err, nodes.ErrNodeTokenInvalid)
}

func TestRecordHeartbeat(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, _ := svc.CreatePairToken(context.Background(), uid)
	node, _, err := svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "rtx"})
	require.NoError(t, err)

	require.NoError(t, svc.RecordHeartbeat(context.Background(), node.ID))

	var status string
	var lastSeen *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status, last_seen_at FROM nodes WHERE id = $1`, node.ID,
	).Scan(&status, &lastSeen))
	require.Equal(t, "online", status)
	require.NotNil(t, lastSeen)
}

func TestUpdateRegister(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, _ := svc.CreatePairToken(context.Background(), uid)
	node, _, err := svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "rtx"})
	require.NoError(t, err)

	require.NoError(t, svc.UpdateRegister(context.Background(),
		node.ID, "0.1.0", []byte(`[{"index":0,"model":"H100","vram_mb":81920}]`)))

	var version string
	var gpu []byte
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT agent_version, gpu_info FROM nodes WHERE id = $1`, node.ID,
	).Scan(&version, &gpu))
	require.Equal(t, "0.1.0", version)
	require.Contains(t, string(gpu), "H100")
}
