//go:build integration

package agentstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

// insertNodeDirect inserts a node row directly into the DB, bypassing the
// pair-token flow. Uses the same pattern as pairNodeHelper in nodes tests.
func insertNodeDirect(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO nodes (owner_user_id, name, agent_version, gpu_info, status, node_token_hash)
		 VALUES ($1, $2, '0.1.0', '[]'::jsonb, 'online', $3)
		 RETURNING id`,
		userID, name, "dummy-hash-"+name+"-"+userID.String(),
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func TestEnvsDispatcher_AutoackMarksRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	pool := newTestPool(t)

	usersSvc := users.NewService(pool)
	user, err := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret123")
	require.NoError(t, err)

	envsSvc := envs.NewService(pool)

	_, err = pool.Exec(ctx, `
		INSERT INTO image_templates (id, display_name, description, image_ref, default_cmd, enabled)
		VALUES ('cuda-base', 'CUDA Base', '', 'flex/dev-cuda-base:dev', '{}', true)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	// Insert a node directly (no headscale, no pair-token flow needed)
	nodesSvc := nodes.NewService(pool)
	nodeID := insertNodeDirect(t, ctx, pool, user.ID, "h20a")

	env, err := envsSvc.Create(ctx, envs.CreateRequest{
		OwnerUserID: user.ID,
		NodeID:      nodeID,
		TemplateID:  "cuda-base",
		Name:        "cuda",
		GPURequest:  1,
	})
	require.NoError(t, err)
	require.Equal(t, "creating", env.Status)

	// Build server + dispatcher. headscale is nil — autoack bypasses it entirely.
	agentSrv := agentstream.NewServer(nodesSvc, envsSvc, usersSvc, sshkeys.NewService(pool), nil)
	disp := agentstream.NewEnvsDispatcher(agentSrv, "http://hs", "flex/sidecar:dev")
	disp.SetAutoack(true)

	require.NoError(t, disp.Create(ctx, env.ID))

	require.Eventually(t, func() bool {
		e, err := envsSvc.ByID(ctx, env.ID)
		return err == nil && e.Status == "running"
	}, 2*time.Second, 100*time.Millisecond, "env should transition to running via autoack")
}
