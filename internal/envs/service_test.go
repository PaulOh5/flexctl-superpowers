package envs_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/users"
)

func newPool(t *testing.T) *pgxpool.Pool {
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
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
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
		CREATE TABLE image_templates (
			id text PRIMARY KEY,
			display_name text NOT NULL,
			description text NOT NULL DEFAULT '',
			image_ref text NOT NULL,
			default_cmd text[] NOT NULL DEFAULT '{}',
			enabled bool NOT NULL DEFAULT true,
			created_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO image_templates (id, display_name, image_ref) VALUES
		  ('cuda-base', 'CUDA Base', 'flex/dev-cuda-base:dev');
		CREATE TABLE envs (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
			template_id text NOT NULL REFERENCES image_templates(id),
			name text NOT NULL,
			hostname text NOT NULL,
			status text NOT NULL DEFAULT 'creating',
			status_message text NOT NULL DEFAULT '',
			sidecar_container_id text NOT NULL DEFAULT '',
			dev_container_id text NOT NULL DEFAULT '',
			gpu_request int NOT NULL DEFAULT 1,
			gpu_indices int[] NOT NULL DEFAULT '{}',
			volume_name text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(),
			UNIQUE(owner_user_id, name)
		);
	`)
	require.NoError(t, err)
	return pool
}

func mkUserAndNode(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	nodesSvc := nodes.NewService(pool)
	tok, _ := nodesSvc.CreatePairToken(context.Background(), u.ID)
	n, _, err := nodesSvc.PairNode(context.Background(), nodes.PairRequest{Token: tok, Name: "rtx"})
	require.NoError(t, err)
	return u.ID, n.ID
}

func TestCreate_HappyPath(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)

	env, err := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid,
		NodeID:      nid,
		TemplateID:  "cuda-base",
		Name:        "vllm-train",
		GPURequest:  1,
	})
	require.NoError(t, err)
	require.Equal(t, "creating", env.Status)
	require.Equal(t, "paul-vllm-train", env.Hostname)
	require.Equal(t, "flex-env-"+env.ID.String(), env.VolumeName)
}

func TestCreate_DuplicateName(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	_, err := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "dup", GPURequest: 1,
	})
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "dup", GPURequest: 1,
	})
	require.ErrorIs(t, err, envs.ErrNameTaken)
}

func TestCreate_InvalidName(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	_, err := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "Bad Name!", GPURequest: 1,
	})
	require.ErrorIs(t, err, envs.ErrInvalidName)
}

func TestMarkRunning(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 2,
	})

	err := svc.MarkRunning(context.Background(), env.ID, "sidecar-id-abc", "dev-id-def", []int{0, 1})
	require.NoError(t, err)

	got, err := svc.ByID(context.Background(), env.ID)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)
	require.Equal(t, "sidecar-id-abc", got.SidecarContainerID)
	require.Equal(t, "dev-id-def", got.DevContainerID)
	require.Equal(t, []int32{0, 1}, got.GPUIndices)
}

func TestMarkError(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})
	require.NoError(t, svc.MarkError(context.Background(), env.ID, "sidecar_start", "container exited"))
	got, _ := svc.ByID(context.Background(), env.ID)
	require.Equal(t, "error", got.Status)
	require.Contains(t, got.StatusMessage, "container exited")
}

func TestMarkStopped(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})
	_ = svc.MarkRunning(context.Background(), env.ID, "s", "d", []int{0})
	require.NoError(t, svc.MarkStopped(context.Background(), env.ID))
	got, _ := svc.ByID(context.Background(), env.ID)
	require.Equal(t, "stopped", got.Status)
	require.Empty(t, got.GPUIndices)
}

func TestListByOwner(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	_, err := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "ea", GPURequest: 1,
	})
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "eb", GPURequest: 1,
	})
	require.NoError(t, err)
	got, err := svc.ListByOwner(context.Background(), uid)
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestDelete(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})
	require.NoError(t, svc.Delete(context.Background(), env.ID))
	_, err := svc.ByID(context.Background(), env.ID)
	require.ErrorIs(t, err, envs.ErrNotFound)
}

func TestListRunningOnNode(t *testing.T) {
	pool := newPool(t)
	svc := envs.NewService(pool)
	uid, nid := mkUserAndNode(t, pool)
	e1, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "ea", GPURequest: 1,
	})
	e2, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "eb", GPURequest: 1,
	})
	_ = svc.MarkRunning(context.Background(), e1.ID, "s1", "d1", []int{0})
	_ = svc.MarkRunning(context.Background(), e2.ID, "s2", "d2", []int{1})
	_ = svc.MarkStopped(context.Background(), e2.ID)

	got, err := svc.ListRunningOnNode(context.Background(), nid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, e1.ID, got[0].ID)
}
