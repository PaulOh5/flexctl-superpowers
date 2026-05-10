package audit_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/paul/flexctl/internal/audit"
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
		CREATE TABLE audit_log (
			id bigserial PRIMARY KEY,
			user_id uuid,
			action text NOT NULL,
			target text,
			metadata jsonb,
			ip inet,
			created_at timestamptz NOT NULL DEFAULT now()
		);
	`)
	require.NoError(t, err)
	return pool
}

func TestLogInserts(t *testing.T) {
	pool := newPool(t)
	uid := uuid.New()
	ip := net.ParseIP("127.0.0.1")

	require.NoError(t, audit.Log(context.Background(), pool, audit.Event{
		UserID:   &uid,
		Action:   "user.signup",
		Target:   "self",
		Metadata: map[string]any{"slug": "paul"},
		IP:       ip,
	}))

	var (
		gotAction string
		gotMeta   []byte
	)
	err := pool.QueryRow(context.Background(),
		`SELECT action, metadata FROM audit_log ORDER BY id DESC LIMIT 1`,
	).Scan(&gotAction, &gotMeta)
	require.NoError(t, err)
	require.Equal(t, "user.signup", gotAction)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(gotMeta, &meta))
	require.Equal(t, "paul", meta["slug"])
}

func TestLogAcceptsNilUser(t *testing.T) {
	pool := newPool(t)
	require.NoError(t, audit.Log(context.Background(), pool, audit.Event{
		Action: "auth.login_failed",
		Target: "p@example.com",
	}))
}
