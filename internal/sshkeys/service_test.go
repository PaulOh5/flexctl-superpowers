package sshkeys_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

const validKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBM5dWmqyhEfP9C1ZDjmh+e9zYx7DbT6JqnNK7NqQy11 paul@laptop"

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
		CREATE TABLE ssh_keys (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name text NOT NULL,
			public_key text NOT NULL,
			fingerprint text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			UNIQUE(user_id, fingerprint)
		);
	`)
	require.NoError(t, err)
	return pool
}

func TestAddKeyAndList(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	svc := sshkeys.NewService(pool)
	k, err := svc.Add(context.Background(), u.ID, "laptop", validKey)
	require.NoError(t, err)
	require.NotEmpty(t, k.Fingerprint)

	keys, err := svc.List(context.Background(), u.ID)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, "laptop", keys[0].Name)
}

func TestAddKeyRejectsBad(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	svc := sshkeys.NewService(pool)
	_, err := svc.Add(context.Background(), u.ID, "bad", "not a real key")
	require.ErrorIs(t, err, sshkeys.ErrInvalidKey)
}

func TestAddKeyRejectsDuplicate(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	svc := sshkeys.NewService(pool)
	_, err := svc.Add(context.Background(), u.ID, "a", validKey)
	require.NoError(t, err)
	_, err = svc.Add(context.Background(), u.ID, "b", validKey)
	require.ErrorIs(t, err, sshkeys.ErrDuplicate)
}

func TestDeleteKey(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	svc := sshkeys.NewService(pool)
	k, _ := svc.Add(context.Background(), u.ID, "laptop", validKey)

	require.NoError(t, svc.Delete(context.Background(), u.ID, k.ID))

	keys, _ := svc.List(context.Background(), u.ID)
	require.Len(t, keys, 0)
}

func TestDeleteKeyRejectsCrossUser(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	a, _ := usersSvc.Signup(context.Background(), "a@x.com", "alice", "correct-horse-battery")
	b, _ := usersSvc.Signup(context.Background(), "b@x.com", "bob", "correct-horse-battery")

	svc := sshkeys.NewService(pool)
	k, _ := svc.Add(context.Background(), a.ID, "alice-key", validKey)

	err := svc.Delete(context.Background(), b.ID, k.ID)
	require.ErrorIs(t, err, sshkeys.ErrNotFound)
}
