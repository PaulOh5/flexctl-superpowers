//go:build integration

package nodes_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/users"
)

// setup starts a Postgres testcontainer, creates the schema, and returns ctx + pool.
func setup(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

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
	return ctx, pool
}

// usersSignupHelper creates a user via users.Service.Signup and returns the user + service.
func usersSignupHelper(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) (users.User, *users.Service) {
	t.Helper()
	svc := users.NewService(pool)
	email := slug + "@example.com"
	u, err := svc.Signup(ctx, email, slug, "supersecret123")
	require.NoError(t, err)
	return u, svc
}

// pairNodeHelper inserts a node row directly into the DB for the given owner.
func pairNodeHelper(t *testing.T, ctx context.Context, svc *nodes.Service, userID uuid.UUID, name string) (uuid.UUID, error) {
	t.Helper()
	pool := nodes.PoolFor(svc)
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO nodes (owner_user_id, name, agent_version, gpu_info, status, node_token_hash)
		 VALUES ($1, $2, '0.1.0', '{}'::jsonb, 'online', $3)
		 RETURNING id`,
		userID, name, "dummy-hash-"+name+"-"+userID.String(),
	).Scan(&id)
	require.NoError(t, err)
	return id, err
}

// mountNodesWithUID builds a chi router that injects uid into the context (bypassing
// session auth) and mounts the authed node routes — mirrors devices.mountWithUID.
func mountNodesWithUID(t *testing.T, svc *nodes.Service, uid uuid.UUID) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.WithUserID(req.Context(), uid)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	nodes.NewHandlers(svc).MountAuthed(r)
	return r
}

func TestList_ReturnsOnlyOwnNodes(t *testing.T) {
	ctx, pool := setup(t)
	u1, _ := usersSignupHelper(t, ctx, pool, "alice")
	u2, _ := usersSignupHelper(t, ctx, pool, "bob")
	svc := nodes.NewService(pool)
	_, _ = pairNodeHelper(t, ctx, svc, u1.ID, "alice-gpu")
	_, _ = pairNodeHelper(t, ctx, svc, u2.ID, "bob-gpu")

	h := mountNodesWithUID(t, svc, u1.ID)
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "alice-gpu", out[0]["name"])
}

func TestList_EmptyForNewUser(t *testing.T) {
	ctx, pool := setup(t)
	u, _ := usersSignupHelper(t, ctx, pool, "newbie")
	svc := nodes.NewService(pool)

	h := mountNodesWithUID(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Equal(t, "[]\n", body)
}
