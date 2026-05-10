package policy_test

import (
	"context"
	"encoding/binary"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/policy"
	"github.com/paul/flexctl/internal/users"
)

func startHeadscale(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	cfgPath, err := filepath.Abs("../../config/headscale-test.yaml")
	require.NoError(t, err)

	req := testcontainers.ContainerRequest{
		Image:        "headscale/headscale:0.23.0",
		ExposedPorts: []string{"8080/tcp"},
		Cmd:          []string{"serve"},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: cfgPath, ContainerFilePath: "/etc/headscale/config.yaml", FileMode: 0o644},
		},
		WaitingFor: wait.ForHTTP("/health").WithPort("8080/tcp").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	baseURL := "http://" + host + ":" + port.Port()

	_, _, err = c.Exec(ctx, []string{"headscale", "users", "create", "control-plane"})
	require.NoError(t, err)
	rc, reader, err := c.Exec(ctx, []string{"headscale", "apikeys", "create", "--expiration", "1h"})
	require.NoError(t, err)
	require.Equal(t, 0, rc)
	apiKey := extractAPIKey(t, readAll(t, reader))
	require.NotEmpty(t, apiKey)
	return baseURL, apiKey
}

// readAll demultiplexes Docker exec stream (8-byte header per frame).
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	var buf []byte
	header := make([]byte, 8)
	for {
		_, err := io.ReadFull(r, header)
		if err != nil {
			break
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		data := make([]byte, size)
		n, err := io.ReadFull(r, data)
		if n > 0 {
			buf = append(buf, data[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

func extractAPIKey(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		token := longestTokenRun(l)
		if len(token) >= 40 {
			return token
		}
	}
	t.Fatalf("could not extract API key from %q", out)
	return ""
}

func longestTokenRun(s string) string {
	var best, cur strings.Builder
	bestLen := 0
	for _, ch := range s {
		isTok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '-'
		if isTok {
			cur.WriteRune(ch)
			if cur.Len() > bestLen {
				bestLen = cur.Len()
				best.Reset()
				best.WriteString(cur.String())
			}
		} else {
			cur.Reset()
		}
	}
	return best.String()
}

func startPostgres(t *testing.T) *pgxpool.Pool {
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
	`)
	require.NoError(t, err)
	return pool
}

func TestPolicyRefresh_NoUsers(t *testing.T) {
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	p := policy.New(pool, hs)
	require.NoError(t, p.Refresh(context.Background()))

	got, err := hs.GetPolicy(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, `"tagOwners"`)
	// no user-derived tags
	require.NotContains(t, got, "tag:device-")
}

func TestPolicyRefresh_OneUser(t *testing.T) {
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	usersSvc := users.NewService(pool)
	_, err := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	p := policy.New(pool, hs)
	require.NoError(t, p.Refresh(context.Background()))

	got, err := hs.GetPolicy(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, "tag:device-paul")
	require.Contains(t, got, "tag:env-paul")
}

func TestPolicyRefresh_AfterUserDeletion(t *testing.T) {
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	_, err = usersSvc.Signup(context.Background(), "a@example.com", "alice", "correct-horse-battery")
	require.NoError(t, err)

	p := policy.New(pool, hs)
	require.NoError(t, p.Refresh(context.Background()))

	// Hard-delete paul, refresh, expect ACL no longer contains paul tags.
	require.NoError(t, usersSvc.HardDelete(context.Background(), u.ID))
	require.NoError(t, p.Refresh(context.Background()))

	got, err := hs.GetPolicy(context.Background())
	require.NoError(t, err)
	require.NotContains(t, got, "tag:device-paul")
	require.NotContains(t, got, "tag:env-paul")
	require.Contains(t, got, "tag:device-alice")
}

func TestPolicyOnUserCreated(t *testing.T) {
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	p := policy.New(pool, hs)
	require.NoError(t, p.OnUserCreated(context.Background(), u.Slug))

	// Headscale user should exist
	hsUsers, err := hs.ListUsers(context.Background())
	require.NoError(t, err)
	names := make([]string, 0, len(hsUsers))
	for _, u := range hsUsers {
		names = append(names, u.Name)
	}
	require.Contains(t, names, "paul")

	// ACL should contain paul's tags
	got, err := hs.GetPolicy(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, "tag:device-paul")
}

func TestPolicyOnUserCreated_DuplicateHeadscaleUserOK(t *testing.T) {
	// If headscale already has the user (e.g. from a previous failed signup),
	// OnUserCreated should still succeed — idempotent.
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	_, err := hs.CreateUser(context.Background(), "paul")
	require.NoError(t, err)

	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	p := policy.New(pool, hs)
	require.NoError(t, p.OnUserCreated(context.Background(), u.Slug))
}

func TestPolicyInitialize_CreatesControlPlaneUser(t *testing.T) {
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)

	// startHeadscale already creates 'control-plane' user via Exec.
	// To test Initialize on its own, delete it first then call Initialize.
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	require.NoError(t, hs.DeleteUser(context.Background(), "control-plane"))

	p := policy.New(pool, hs)
	require.NoError(t, p.Initialize(context.Background()))

	userList, err := hs.ListUsers(context.Background())
	require.NoError(t, err)
	names := make([]string, 0, len(userList))
	for _, u := range userList {
		names = append(names, u.Name)
	}
	require.Contains(t, names, "control-plane")
}

func TestPolicyInitialize_Idempotent(t *testing.T) {
	pool := startPostgres(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	p := policy.New(pool, hs)
	require.NoError(t, p.Initialize(context.Background()))
	require.NoError(t, p.Initialize(context.Background()))
}
