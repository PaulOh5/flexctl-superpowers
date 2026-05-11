package imagetemplates_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/paul/flexctl/internal/imagetemplates"
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
		  ('cuda-base', 'CUDA Base', 'flex/dev-cuda-base:dev'),
		  ('disabled-x', 'Disabled', 'x') ;
		UPDATE image_templates SET enabled=false WHERE id='disabled-x';
	`)
	require.NoError(t, err)
	return pool
}

func TestList_OnlyEnabled(t *testing.T) {
	pool := newPool(t)
	svc := imagetemplates.NewService(pool)
	got, err := svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "cuda-base", got[0].ID)
}

func TestByID_Enabled(t *testing.T) {
	pool := newPool(t)
	svc := imagetemplates.NewService(pool)
	tpl, err := svc.ByID(context.Background(), "cuda-base")
	require.NoError(t, err)
	require.Equal(t, "flex/dev-cuda-base:dev", tpl.ImageRef)
}

func TestByID_DisabledFails(t *testing.T) {
	pool := newPool(t)
	svc := imagetemplates.NewService(pool)
	_, err := svc.ByID(context.Background(), "disabled-x")
	require.ErrorIs(t, err, imagetemplates.ErrNotFound)
}

func TestByID_MissingFails(t *testing.T) {
	pool := newPool(t)
	svc := imagetemplates.NewService(pool)
	_, err := svc.ByID(context.Background(), "ghost")
	require.ErrorIs(t, err, imagetemplates.ErrNotFound)
}
