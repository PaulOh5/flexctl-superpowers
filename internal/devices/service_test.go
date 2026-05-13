//go:build integration

package devices_test

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

	"github.com/paul/flexctl/internal/devices"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/policy"
	"github.com/paul/flexctl/internal/users"
)

// newPool starts a Postgres testcontainer and creates the tables needed by devices.
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

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE TABLE users (
			id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
			email        text        NOT NULL UNIQUE,
			slug         text        NOT NULL UNIQUE,
			password_hash text       NOT NULL,
			created_at   timestamptz NOT NULL DEFAULT now()
		);
		CREATE INDEX users_email_lower_idx ON users (lower(email));
		CREATE TABLE devices (
			id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name         text        NOT NULL,
			hostname     text        NOT NULL UNIQUE,
			created_at   timestamptz NOT NULL DEFAULT now(),
			last_seen_at timestamptz,
			UNIQUE(user_id, name)
		);
		CREATE INDEX devices_user_id_idx ON devices(user_id);
	`)
	require.NoError(t, err)
	return pool
}

// startHeadscale starts a Headscale 0.23.0 testcontainer and returns (baseURL, apiKey).
// Mirrors internal/headscale/testhelpers_test.go.
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
			{
				HostFilePath:      cfgPath,
				ContainerFilePath: "/etc/headscale/config.yaml",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForHTTP("/health").
			WithPort("8080/tcp").
			WithStartupTimeout(60 * time.Second),
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

	_, _, err = c.Exec(ctx, []string{"headscale", "users", "create", "control-plane"})
	require.NoError(t, err)
	rc, reader, err := c.Exec(ctx, []string{"headscale", "apikeys", "create", "--expiration", "1h"})
	require.NoError(t, err)
	require.Equal(t, 0, rc)
	apiKey := extractAPIKey(t, readDockerStream(reader))

	baseURL := "http://" + host + ":" + port.Port()
	return baseURL, apiKey
}

// setup returns ctx + pool + headscale client + users svc.
func setup(t *testing.T) (context.Context, *pgxpool.Pool, *headscale.Client, *users.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	pool := newPool(t)
	baseURL, apiKey := startHeadscale(t)
	hs := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	require.NoError(t, policy.New(pool, hs).Initialize(ctx))

	usersSvc := users.NewService(pool)
	return ctx, pool, hs, usersSvc
}

func TestPair_CreatesDeviceAndPreAuthKey(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)

	u, err := usersSvc.Signup(ctx, "paul@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	// Create headscale user so the pre-auth key can be created.
	_, err = hs.CreateUser(ctx, u.Slug)
	require.NoError(t, err)

	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "http://headscale:8080",
		TailnetDomain:      "flex.local",
	})

	res, err := svc.Pair(ctx, u.ID, "macbook")
	require.NoError(t, err)
	require.Equal(t, "paul-device-macbook", res.Device.Hostname)
	require.NotEmpty(t, res.PreauthKey)
	require.Equal(t, "http://headscale:8080", res.HeadscaleURL)
	require.Equal(t, "flex.local", res.TailnetDomain)

	// Device row must exist in DB.
	got, err := svc.ByID(ctx, res.Device.ID)
	require.NoError(t, err)
	require.Equal(t, u.ID, got.UserID)
	require.Equal(t, "macbook", got.Name)

	// Idempotent on (user, name): same device row, new pre-auth key.
	res2, err := svc.Pair(ctx, u.ID, "macbook")
	require.NoError(t, err)
	require.Equal(t, res.Device.ID, res2.Device.ID)
	require.NotEqual(t, res.PreauthKey, res2.PreauthKey)
}

func TestPair_RejectsInvalidName(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)

	u, err := usersSvc.Signup(ctx, "paul@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{})

	_, err = svc.Pair(ctx, u.ID, "Bad Name!")
	require.ErrorIs(t, err, devices.ErrInvalidName)

	_, err = svc.Pair(ctx, u.ID, "a") // too short (regex requires len >= 2)
	require.ErrorIs(t, err, devices.ErrInvalidName)
}

func TestList_ScopedToUser(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)

	u1, err := usersSvc.Signup(ctx, "u1@example.com", "alice", "correct-horse-battery")
	require.NoError(t, err)
	u2, err := usersSvc.Signup(ctx, "u2@example.com", "bob", "correct-horse-battery")
	require.NoError(t, err)

	_, err = hs.CreateUser(ctx, u1.Slug)
	require.NoError(t, err)
	_, err = hs.CreateUser(ctx, u2.Slug)
	require.NoError(t, err)

	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{})

	_, err = svc.Pair(ctx, u1.ID, "laptop")
	require.NoError(t, err)
	_, err = svc.Pair(ctx, u1.ID, "desktop")
	require.NoError(t, err)
	_, err = svc.Pair(ctx, u2.ID, "tablet")
	require.NoError(t, err)

	list1, err := svc.List(ctx, u1.ID)
	require.NoError(t, err)
	require.Len(t, list1, 2)

	list2, err := svc.List(ctx, u2.ID)
	require.NoError(t, err)
	require.Len(t, list2, 1)
}

// Note: the device hasn't actually registered with Headscale in this test
// (that would require `tailscale up`), so only the DB row deletion is verified.
func TestDelete_RemovesDBRow(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)

	u, err := usersSvc.Signup(ctx, "paul@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	_, err = hs.CreateUser(ctx, u.Slug)
	require.NoError(t, err)

	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{})

	res, err := svc.Pair(ctx, u.ID, "macbook")
	require.NoError(t, err)

	require.NoError(t, svc.Delete(ctx, u.ID, res.Device.ID))

	_, err = svc.ByID(ctx, res.Device.ID)
	require.ErrorIs(t, err, devices.ErrNotFound)
}

func TestDelete_RejectsCrossUser(t *testing.T) {
	ctx, pool, hs, usersSvc := setup(t)

	u1, err := usersSvc.Signup(ctx, "u1@example.com", "alice", "correct-horse-battery")
	require.NoError(t, err)
	u2, err := usersSvc.Signup(ctx, "u2@example.com", "bob", "correct-horse-battery")
	require.NoError(t, err)

	_, err = hs.CreateUser(ctx, u1.Slug)
	require.NoError(t, err)

	svc := devices.NewService(pool, hs, usersSvc, devices.ServiceConfig{})

	res, err := svc.Pair(ctx, u1.ID, "laptop")
	require.NoError(t, err)

	// u2 tries to delete u1's device — must be rejected.
	err = svc.Delete(ctx, u2.ID, res.Device.ID)
	require.ErrorIs(t, err, devices.ErrNotFound)

	// Device must still exist.
	_, err = svc.ByID(ctx, res.Device.ID)
	require.NoError(t, err)
}

// readDockerStream demultiplexes the Docker exec multiplexed stream (8-byte headers).
func readDockerStream(r io.Reader) string {
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
		_, err = io.ReadFull(r, data)
		if err != nil {
			buf = append(buf, data...)
			break
		}
		buf = append(buf, data...)
	}
	return string(buf)
}

func extractAPIKey(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		token := longestTokenRun(l)
		if len(token) >= 40 {
			return token
		}
	}
	t.Fatalf("could not extract API key from output: %q", out)
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
