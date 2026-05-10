package users_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

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
		CREATE INDEX users_email_lower_idx ON users (lower(email));
	`)
	require.NoError(t, err)
	return pool
}

func TestSignupCreatesUser(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)

	u, err := svc.Signup(context.Background(), "Paul@Example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	require.NotEqual(t, "", u.ID.String())
	require.Equal(t, "paul@example.com", u.Email)
	require.Equal(t, "paul", u.Slug)
}

func TestSignupRejectsDuplicateEmail(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	ctx := context.Background()

	_, err := svc.Signup(ctx, "a@b.com", "alice", "password-aaaaaaaa")
	require.NoError(t, err)

	_, err = svc.Signup(ctx, "A@B.COM", "alice2", "password-bbbbbbbb")
	require.ErrorIs(t, err, users.ErrEmailTaken)
}

func TestSignupRejectsDuplicateSlug(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	ctx := context.Background()

	_, err := svc.Signup(ctx, "a@b.com", "alice", "password-aaaaaaaa")
	require.NoError(t, err)

	_, err = svc.Signup(ctx, "c@d.com", "alice", "password-bbbbbbbb")
	require.ErrorIs(t, err, users.ErrSlugTaken)
}

func TestSignupRejectsBadInputs(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	ctx := context.Background()

	_, err := svc.Signup(ctx, "not-an-email", "ok", "password-aaaaaaaa")
	require.ErrorIs(t, err, users.ErrInvalidEmail)

	_, err = svc.Signup(ctx, "a@b.com", "Bad Slug!", "password-aaaaaaaa")
	require.ErrorIs(t, err, users.ErrInvalidSlug)

	_, err = svc.Signup(ctx, "a@b.com", "ok", "short")
	require.ErrorIs(t, err, users.ErrPasswordTooShort)
}
