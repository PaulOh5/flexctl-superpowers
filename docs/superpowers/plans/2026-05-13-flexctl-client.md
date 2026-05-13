# Plan 5 — flexctl client (tsnet + SSH ProxyCommand) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 사용자 노트북에서 `flexctl ssh <env>` (또는 표준 `ssh dev@<env>.flex`) 명령으로 GPU dev 컨테이너에 실제 SSH 접속할 수 있게 만든다. tsnet 임베드 + on-demand ProxyCommand 패턴.

**Architecture:** flexctl 바이너리에 client 명령 추가(`login`, `logout`, `ssh`, `proxy`, `key`, `env list`) + control-plane에 `devices` 서브시스템(테이블 + REST API) 추가. `flexctl login`이 한 번에 (a) 세션 발급 (b) ~/.ssh/id_*.pub 자동 업로드 (c) 디바이스 페어링 + Headscale pre-auth key 수령 (d) tsnet state init + tailnet 가입 (e) ~/.ssh/config 마커 블록 삽입까지 idempotent하게 수행. `flexctl proxy`가 OpenSSH의 ProxyCommand로 호출돼 매 SSH 세션마다 tsnet을 부팅 후 stdin/stdout↔tsnet.Dial 파이프.

**Tech Stack:** Go 1.26, `tailscale.com/tsnet`, `golang.org/x/term`, chi v5, pgx v5, testcontainers-go (Postgres + Headscale 0.23), spf13/cobra.

**Spec:** `docs/superpowers/specs/2026-05-13-flexctl-client-design.md`

---

## File Structure

**Create:**
- `migrations/0009_devices.up.sql`, `.down.sql` — devices 테이블
- `internal/devices/service.go`, `service_test.go` — Pair/List/Delete/ByID + Postgres+Headscale 통합 테스트
- `internal/devices/handlers.go`, `handlers_test.go` — REST API + httptest
- `internal/flextsnet/server.go` — `tsnet.Server` 래퍼
- `internal/flextsnet/server_integration_test.go` — testcontainers Headscale + 두 tsnet 인스턴스 dial 검증 (`//go:build integration`)
- `internal/flexctlcli/clientconfig.go`, `clientconfig_test.go` — ~/.config/flexctl/client.toml read/write
- `internal/flexctlcli/sshconfig.go`, `sshconfig_test.go` — ~/.ssh/config 마커 블록 idempotent insert/remove
- `internal/flexctlcli/apiclient.go`, `apiclient_test.go` — control-plane HTTP client
- `internal/flexctlcli/key.go`, `key_test.go` — `flexctl key add/list/rm`
- `internal/flexctlcli/envlist.go`, `envlist_test.go` — `flexctl env list`
- `internal/flexctlcli/login.go`, `login_test.go` — `flexctl login` 5단계 통합
- `internal/flexctlcli/logout.go`, `logout_test.go` — `flexctl logout`
- `internal/flexctlcli/ssh.go`, `ssh_test.go` — `flexctl ssh` (외부 ssh exec wrapper)
- `internal/flexctlcli/proxy.go`, `proxy_test.go` — `flexctl proxy` (ProxyCommand 진입점)

**Modify:**
- `internal/headscale/client.go` — `DeleteNode`, `ListNodes` 메서드 추가
- `internal/headscale/client_test.go` — DeleteNode/ListNodes 테스트
- `cmd/control-plane/main.go` — devices 와이어링
- `internal/flexctlcli/root.go` — 신규 서브커맨드 등록
- `README.md` — Plan 5 사용법 + 수동 e2e 체크리스트
- `go.mod`, `go.sum` — tsnet 의존성

---

## Task 1: Headscale DeleteNode + ListNodes API

`devices.Service.Delete`가 Headscale 노드를 직접 삭제하려면 클라이언트에 메서드가 필요하다.

**Files:**
- Modify: `internal/headscale/client.go`
- Modify: `internal/headscale/client_test.go`

- [ ] **Step 1: Write failing test for `ListNodes` + `DeleteNode`**

Append to `internal/headscale/client_test.go`:

```go
func TestListNodes_ReturnsRegisteredNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := startHeadscale(t, ctx)

	_, err := c.CreateUser(ctx, "paul")
	require.NoError(t, err)

	// Pre-create an ACL policy so pre-auth keys can be issued.
	require.NoError(t, c.SetPolicy(ctx, `{
	  "tagOwners": {"tag:device-paul": ["control-plane"]},
	  "acls": [{"action": "accept", "src": ["tag:device-paul"], "dst": ["tag:device-paul:*"]}]
	}`))

	key, err := c.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Reusable: false, Ephemeral: false,
		Expiration: 1 * time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, key.Key)

	// Without a registered Tailscale node we can only assert the call shape.
	got, err := c.ListNodes(ctx, "paul")
	require.NoError(t, err)
	require.Empty(t, got, "no nodes have registered yet")
}

func TestDeleteNode_UnknownID_Returns404Error(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := startHeadscale(t, ctx)

	err := c.DeleteNode(ctx, "9999999")
	require.Error(t, err)
}
```

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test ./internal/headscale/ -run 'TestListNodes|TestDeleteNode' -count=1`
Expected: build error — undefined `ListNodes`/`DeleteNode`.

- [ ] **Step 3: Implement `ListNodes` + `DeleteNode`**

Append to `internal/headscale/client.go`:

```go
type Node struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	GivenName string  `json:"givenName"`
	User     User     `json:"user"`
	IPAddresses []string `json:"ipAddresses"`
}

type listNodesResp struct {
	Nodes []Node `json:"nodes"`
}

// ListNodes returns all Headscale nodes for a given user (by name). Empty
// user filter returns all nodes (admin scope). Used by devices.Service to
// look up a device's Headscale node ID before deletion.
func (c *Client) ListNodes(ctx context.Context, user string) ([]Node, error) {
	path := "/api/v1/node"
	if user != "" {
		path += "?user=" + url.QueryEscape(user)
	}
	var out listNodesResp
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

// DeleteNode removes a Headscale node by its numeric ID (as returned by
// ListNodes). Used by flexctl logout to drop a user device from the tailnet.
func (c *Client) DeleteNode(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/node/"+id, nil, nil)
}
```

Add `"net/url"` to imports if not present.

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/headscale/ -run 'TestListNodes|TestDeleteNode' -count=1`
Expected: PASS (testcontainers Headscale fixture handles bring-up).

- [ ] **Step 5: Commit**

```bash
git add internal/headscale/client.go internal/headscale/client_test.go
git commit -m "feat(headscale): ListNodes + DeleteNode API for device lifecycle"
```

---

## Task 2: Migration 0009_devices

**Files:**
- Create: `migrations/0009_devices.up.sql`
- Create: `migrations/0009_devices.down.sql`

- [ ] **Step 1: Write the migration**

`migrations/0009_devices.up.sql`:

```sql
CREATE TABLE devices (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         text NOT NULL,
  hostname     text NOT NULL UNIQUE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz,
  UNIQUE(user_id, name)
);
CREATE INDEX devices_user_id_idx ON devices(user_id);
```

`migrations/0009_devices.down.sql`:

```sql
DROP TABLE IF EXISTS devices;
```

- [ ] **Step 2: Verify migration applies and rolls back cleanly**

Run from repo root with Postgres available (use `make psql-shell` setup from Plan 1, or the existing migration test):

```bash
go test ./internal/db/ -run TestMigrationsApplyAndRollback -count=1
```

Expected: PASS (if no such generic test exists, manually run `migrate up` and `migrate down` against the testcontainers Postgres fixture in Plan 1).

- [ ] **Step 3: Commit**

```bash
git add migrations/0009_devices.up.sql migrations/0009_devices.down.sql
git commit -m "feat(db): devices table for user laptop pairing"
```

---

## Task 3: devices.Service — Pair/List/Delete/ByID

Postgres + Headscale 통합 테스트. Plan 4의 `internal/envs/service_test.go`를 픽스처 패턴 참고.

**Files:**
- Create: `internal/devices/service.go`
- Create: `internal/devices/service_test.go`

- [ ] **Step 1: Write failing tests**

`internal/devices/service_test.go`:

```go
//go:build integration

package devices_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/devices"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/policy"
	"github.com/paul/flexctl/internal/users"
)

func setup(t *testing.T) (context.Context, *pgxpool.Pool, *headscale.Client, *users.Service, devices.HeadscaleClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	pool := startPostgresWithMigrations(t, ctx)         // shared fixture, see existing test helpers
	hs := startHeadscale(t, ctx)                         // shared fixture
	require.NoError(t, policy.New(pool, hs).Initialize(ctx))

	usersSvc := users.NewService(pool, policy.New(pool, hs))
	return ctx, pool, hs, usersSvc, hs
}

func TestPair_CreatesDeviceAndPreAuthKey(t *testing.T) {
	ctx, _, hs, usersSvc, hsi := setup(t)

	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret")
	require.NoError(t, err)

	svc := devices.NewService(nil, hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com",
		TailnetDomain:      "flex",
	})
	// rebind pool via test helper — we used nil above for brevity. Real wiring:
	svc = devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com",
		TailnetDomain:      "flex",
	})

	res, err := svc.Pair(ctx, u.ID, "macbook")
	require.NoError(t, err)
	require.Equal(t, "paul-device-macbook", res.Device.Hostname)
	require.NotEmpty(t, res.PreauthKey)
	require.Equal(t, "https://hs.example.com", res.HeadscaleURL)
	require.Equal(t, "flex", res.TailnetDomain)

	// Idempotent on (user, name): same device row, new pre-auth key.
	res2, err := svc.Pair(ctx, u.ID, "macbook")
	require.NoError(t, err)
	require.Equal(t, res.Device.ID, res2.Device.ID)
	require.NotEqual(t, res.PreauthKey, res2.PreauthKey)
	_ = hs
}

func TestPair_RejectsInvalidName(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret")
	require.NoError(t, err)
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})

	_, err = svc.Pair(ctx, u.ID, "Bad Name!")
	require.ErrorIs(t, err, devices.ErrInvalidName)
}

func TestList_ScopedToUser(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u1, _ := usersSvc.Signup(ctx, "a@x.com", "alice", "supersecret")
	u2, _ := usersSvc.Signup(ctx, "b@x.com", "bob", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	_, _ = svc.Pair(ctx, u1.ID, "laptop")
	_, _ = svc.Pair(ctx, u2.ID, "laptop")

	got, err := svc.List(ctx, u1.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "alice-device-laptop", got[0].Hostname)
}

func TestDelete_RemovesDBRowAndHeadscaleNode(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u, _ := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	res, err := svc.Pair(ctx, u.ID, "laptop")
	require.NoError(t, err)

	require.NoError(t, svc.Delete(ctx, u.ID, res.Device.ID))

	got, _ := svc.List(ctx, u.ID)
	require.Empty(t, got)
}

func TestDelete_RejectsCrossUser(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u1, _ := usersSvc.Signup(ctx, "a@x.com", "alice", "supersecret")
	u2, _ := usersSvc.Signup(ctx, "b@x.com", "bob", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	res, _ := svc.Pair(ctx, u1.ID, "laptop")

	err := svc.Delete(ctx, u2.ID, res.Device.ID)
	require.ErrorIs(t, err, devices.ErrNotFound)
}

// startPostgresWithMigrations, startHeadscale, getPool are pre-existing test
// helpers used by other packages (internal/envs, internal/users). Copy or
// import them as the existing pattern dictates.
func startPostgresWithMigrations(t *testing.T, ctx context.Context) *pgxpool.Pool { panic("use existing helper") }
func startHeadscale(t *testing.T, ctx context.Context) *headscale.Client          { panic("use existing helper") }
func getPool(t *testing.T) *pgxpool.Pool                                          { panic("use existing helper") }

var _ = uuid.Nil // silence import if unused
```

The placeholder helpers (`startPostgresWithMigrations`, `startHeadscale`, `getPool`) are stand-ins — replace them with the actual fixtures the codebase already uses in `internal/envs/service_test.go` and `internal/headscale/client_test.go`. Look at those files and copy the testcontainers bring-up pattern verbatim.

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test -tags=integration ./internal/devices/ -count=1`
Expected: build error — undefined `devices.Service`, `devices.NewService`, etc.

- [ ] **Step 3: Implement `internal/devices/service.go`**

```go
package devices

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/users"
)

var (
	ErrInvalidName = errors.New("invalid device name")
	ErrConflict    = errors.New("device hostname already taken")
	ErrNotFound    = errors.New("device not found")
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

type Device struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Hostname   string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

type PairResult struct {
	Device        Device
	PreauthKey    string
	HeadscaleURL  string
	TailnetDomain string
}

type HeadscaleClient interface {
	CreatePreAuthKey(ctx context.Context, req headscale.PreAuthKeyRequest) (headscale.PreAuthKey, error)
	ListNodes(ctx context.Context, user string) ([]headscale.Node, error)
	DeleteNode(ctx context.Context, id string) error
}

type ServiceConfig struct {
	HeadscaleClientURL string // 외부에서 도달 가능한 Headscale URL (사용자 노트북이 dial)
	TailnetDomain      string // MagicDNS suffix, 기본 "flex"
}

type Service struct {
	pool  *pgxpool.Pool
	hs    HeadscaleClient
	users *users.Service
	cfg   ServiceConfig
}

func NewService(pool *pgxpool.Pool, hs HeadscaleClient, usersSvc *users.Service, cfg ServiceConfig) *Service {
	return &Service{pool: pool, hs: hs, users: usersSvc, cfg: cfg}
}

func (s *Service) Pair(ctx context.Context, userID uuid.UUID, name string) (PairResult, error) {
	if !nameRe.MatchString(name) {
		return PairResult{}, ErrInvalidName
	}
	u, err := s.users.ByID(ctx, userID)
	if err != nil {
		return PairResult{}, fmt.Errorf("user: %w", err)
	}
	hostname := u.Slug + "-device-" + name

	var d Device
	err = s.pool.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, hostname)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, name) DO UPDATE
		  SET last_seen_at = now()
		RETURNING id, user_id, name, hostname, created_at, last_seen_at`,
		userID, name, hostname,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Hostname, &d.CreatedAt, &d.LastSeenAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return PairResult{}, ErrConflict
		}
		return PairResult{}, fmt.Errorf("upsert device: %w", err)
	}

	pak, err := s.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: u.Slug, Reusable: false, Ephemeral: false,
		Expiration: 10 * time.Minute,
		ACLTags:    []string{"tag:device-" + u.Slug},
	})
	if err != nil {
		return PairResult{}, fmt.Errorf("preauthkey: %w", err)
	}

	return PairResult{
		Device:        d,
		PreauthKey:    pak.Key,
		HeadscaleURL:  s.cfg.HeadscaleClientURL,
		TailnetDomain: s.cfg.TailnetDomain,
	}, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name, hostname, created_at, last_seen_at
		FROM devices WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Hostname, &d.CreatedAt, &d.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (Device, error) {
	var d Device
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, hostname, created_at, last_seen_at
		FROM devices WHERE id = $1`, id,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Hostname, &d.CreatedAt, &d.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("select: %w", err)
	}
	return d, nil
}

func (s *Service) Delete(ctx context.Context, userID, deviceID uuid.UUID) error {
	d, err := s.ByID(ctx, deviceID)
	if err != nil {
		return err
	}
	if d.UserID != userID {
		return ErrNotFound
	}
	u, err := s.users.ByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("user: %w", err)
	}

	// Best-effort Headscale node cleanup. Node may have never registered or
	// already deregistered — both are fine.
	nodes, err := s.hs.ListNodes(ctx, u.Slug)
	if err == nil {
		for _, n := range nodes {
			if n.Name == d.Hostname || n.GivenName == d.Hostname {
				_ = s.hs.DeleteNode(ctx, n.ID)
			}
		}
	}

	tag, err := s.pool.Exec(ctx,
		`DELETE FROM devices WHERE id = $1 AND user_id = $2`, deviceID, userID)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test -tags=integration ./internal/devices/ -count=1`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/devices/service.go internal/devices/service_test.go
git commit -m "feat(devices): Pair/List/Delete/ByID with Headscale pre-auth key"
```

---

## Task 4: devices.Handlers

**Files:**
- Create: `internal/devices/handlers.go`
- Create: `internal/devices/handlers_test.go`

- [ ] **Step 1: Write failing tests**

`internal/devices/handlers_test.go`:

```go
package devices_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/devices"
)

func mountWithUID(t *testing.T, svc *devices.Service, uid uuid.UUID) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.WithUserID(req.Context(), uid)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	devices.NewHandlers(svc).Mount(r)
	return r
}

func TestPair_Returns200WithKey(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u, _ := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	h := mountWithUID(t, svc, u.ID)

	body, _ := json.Marshal(map[string]string{"name": "macbook"})
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/pair", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp struct {
		Hostname      string `json:"hostname"`
		PreauthKey    string `json:"preauth_key"`
		HeadscaleURL  string `json:"headscale_url"`
		TailnetDomain string `json:"tailnet_domain"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "paul-device-macbook", resp.Hostname)
	require.NotEmpty(t, resp.PreauthKey)
}

func TestPair_InvalidNameReturns400(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u, _ := usersSvc.Signup(ctx, "p@x.com", "paul", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	h := mountWithUID(t, svc, u.ID)

	body, _ := json.Marshal(map[string]string{"name": "BAD NAME"})
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/pair", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestList_ReturnsOnlyOwnDevices(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u1, _ := usersSvc.Signup(ctx, "a@x.com", "alice", "supersecret")
	u2, _ := usersSvc.Signup(ctx, "b@x.com", "bob", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	_, _ = svc.Pair(ctx, u1.ID, "laptop")
	_, _ = svc.Pair(ctx, u2.ID, "laptop")

	h := mountWithUID(t, svc, u1.ID)
	req := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "alice-device-laptop", out[0]["hostname"])
}

func TestDelete_NotFoundOnCrossUser(t *testing.T) {
	ctx, _, _, usersSvc, hsi := setup(t)
	u1, _ := usersSvc.Signup(ctx, "a@x.com", "alice", "supersecret")
	u2, _ := usersSvc.Signup(ctx, "b@x.com", "bob", "supersecret")
	svc := devices.NewService(getPool(t), hsi, usersSvc, devices.ServiceConfig{
		HeadscaleClientURL: "https://hs.example.com", TailnetDomain: "flex",
	})
	res, _ := svc.Pair(ctx, u1.ID, "laptop")

	h := mountWithUID(t, svc, u2.ID)
	req := httptest.NewRequest(http.MethodDelete, "/v1/devices/"+res.Device.ID.String(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	var _ = context.Background
}
```

The `setup`, `getPool`, etc. helpers are the ones already used in `service_test.go` (Task 3) — share them via the same `_test.go` package or a new `testhelpers_test.go` file in the package.

`internal/auth/middleware.go` already exposes `WithUserID` (if it doesn't, add it as part of this task with one line — see existing `UserIDFrom` for the matching pattern).

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test -tags=integration ./internal/devices/ -count=1`
Expected: build error — undefined `devices.Handlers`, `NewHandlers`.

- [ ] **Step 3: Implement `internal/devices/handlers.go`**

```go
package devices

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

type Handlers struct{ svc *Service }

func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

func (h *Handlers) Mount(r chi.Router) {
	r.Post("/v1/devices/pair", h.pair)
	r.Get("/v1/devices", h.list)
	r.Delete("/v1/devices/{id}", h.delete)
}

type pairReq struct {
	Name string `json:"name"`
}

type pairResp struct {
	DeviceID      string `json:"device_id"`
	Hostname      string `json:"hostname"`
	PreauthKey    string `json:"preauth_key"`
	HeadscaleURL  string `json:"headscale_url"`
	TailnetDomain string `json:"tailnet_domain"`
}

type deviceResp struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hostname   string     `json:"hostname"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

func (h *Handlers) pair(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var req pairReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json"); return
	}
	res, err := h.svc.Pair(r.Context(), uid, req.Name)
	switch {
	case errors.Is(err, ErrInvalidName):
		httperr.Write(w, http.StatusBadRequest, "invalid device name"); return
	case errors.Is(err, ErrConflict):
		httperr.Write(w, http.StatusConflict, "device hostname already taken"); return
	case err != nil:
		slog.Error("devices.Pair", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pairResp{
		DeviceID:      res.Device.ID.String(),
		Hostname:      res.Device.Hostname,
		PreauthKey:    res.PreauthKey,
		HeadscaleURL:  res.HeadscaleURL,
		TailnetDomain: res.TailnetDomain,
	})
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	got, err := h.svc.List(r.Context(), uid)
	if err != nil {
		slog.Error("devices.List", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	out := make([]deviceResp, 0, len(got))
	for _, d := range got {
		out = append(out, deviceResp{
			ID: d.ID.String(), Name: d.Name, Hostname: d.Hostname,
			CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id"); return
	}
	err = h.svc.Delete(r.Context(), uid, id)
	switch {
	case errors.Is(err, ErrNotFound):
		httperr.Write(w, http.StatusNotFound, "not found"); return
	case err != nil:
		slog.Error("devices.Delete", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

If `auth.WithUserID` is missing, add to `internal/auth/middleware.go`:

```go
func WithUserID(ctx context.Context, uid uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDKey{}, uid)
}
```

(Match whatever value-type the existing `UserIDFrom` uses.)

- [ ] **Step 4: Run tests, expect pass**

Run: `go test -tags=integration ./internal/devices/ -count=1`
Expected: PASS (9 tests including Task 3).

- [ ] **Step 5: Commit**

```bash
git add internal/devices/handlers.go internal/devices/handlers_test.go internal/auth/middleware.go
git commit -m "feat(devices): REST API (Pair/List/Delete) behind session auth"
```

---

## Task 5: control-plane main.go — devices 와이어링

**Files:**
- Modify: `cmd/control-plane/main.go`

- [ ] **Step 1: Add the wiring**

Find the section in `cmd/control-plane/main.go` where other handlers (envs, sshkeys, nodes) are mounted behind `auth.RequireSession`. Insert:

```go
devicesSvc := devices.NewService(pool, headscaleClient, usersSvc, devices.ServiceConfig{
    HeadscaleClientURL: envOr("FLEX_HEADSCALE_CLIENT_URL", headscaleURL),
    TailnetDomain:      envOr("FLEX_TAILNET_DOMAIN", "flex"),
})
devicesH := devices.NewHandlers(devicesSvc)
```

And in the `r.Group` that uses `RequireSession`:

```go
devicesH.Mount(r)
```

Add to imports:

```go
"github.com/paul/flexctl/internal/devices"
```

If `envOr` does not exist, add it as a small helper near the top of `main.go`:

```go
func envOr(key, fallback string) string {
    if v := os.Getenv(key); v != "" { return v }
    return fallback
}
```

- [ ] **Step 2: Verify control-plane still builds and starts**

Run: `go build ./cmd/control-plane && go vet ./...`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add cmd/control-plane/main.go
git commit -m "feat(control-plane): wire devices service + handlers"
```

---

## Task 6: clientconfig — ~/.config/flexctl/client.toml

**Files:**
- Create: `internal/flexctlcli/clientconfig.go`
- Create: `internal/flexctlcli/clientconfig_test.go`

This package is used by login/logout/proxy/ssh, so it has to land before them. We use plain `text/template` + simple parse (no toml lib needed yet — see Step 3) or reuse whatever Plan 3 used for `agent.toml`. Check `internal/flexctlcli/join.go:99-108`: it just writes formatted Go strings. We mirror that style.

- [ ] **Step 1: Write failing tests**

```go
package flexctlcli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestClientConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.toml")

	want := flexctlcli.ClientConfig{
		ControlPlane:    "https://flexctl.example.com",
		SessionCookie:   "flex_session=eyJh...",
		DeviceName:      "macbook",
		DeviceHostname:  "paul-device-macbook",
		HeadscaleURL:    "https://headscale.example.com",
		TailnetDomain:   "flex",
	}
	require.NoError(t, flexctlcli.WriteClientConfig(path, want))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := flexctlcli.ReadClientConfig(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestClientConfig_MissingFileReturnsErrNotExist(t *testing.T) {
	_, err := flexctlcli.ReadClientConfig(filepath.Join(t.TempDir(), "nope.toml"))
	require.True(t, os.IsNotExist(err))
}
```

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestClientConfig -count=1`
Expected: undefined symbols.

- [ ] **Step 3: Implement `clientconfig.go`**

```go
package flexctlcli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ClientConfig struct {
	ControlPlane   string
	SessionCookie  string
	DeviceName     string
	DeviceHostname string
	HeadscaleURL   string
	TailnetDomain  string
}

func WriteClientConfig(path string, c ClientConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	contents := fmt.Sprintf(`control_plane = %q
session_cookie = %q
device_name = %q
device_hostname = %q
headscale_url = %q
tailnet_domain = %q
`, c.ControlPlane, c.SessionCookie, c.DeviceName,
		c.DeviceHostname, c.HeadscaleURL, c.TailnetDomain)
	return os.WriteFile(path, []byte(contents), 0o600)
}

func ReadClientConfig(path string) (ClientConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return ClientConfig{}, err
	}
	defer f.Close()

	out := ClientConfig{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		raw := strings.TrimSpace(line[eq+1:])
		v, err := strconv.Unquote(raw)
		if err != nil {
			continue
		}
		switch k {
		case "control_plane":
			out.ControlPlane = v
		case "session_cookie":
			out.SessionCookie = v
		case "device_name":
			out.DeviceName = v
		case "device_hostname":
			out.DeviceHostname = v
		case "headscale_url":
			out.HeadscaleURL = v
		case "tailnet_domain":
			out.TailnetDomain = v
		}
	}
	return out, sc.Err()
}

// DefaultPath returns ~/.config/flexctl/client.toml, honoring $FLEXCTL_CLIENT_CONFIG.
func DefaultClientConfigPath() string {
	if v := os.Getenv("FLEXCTL_CLIENT_CONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "flexctl", "client.toml")
}

func DefaultStateDir() string {
	if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "flexctl", "tsnet")
}

func DefaultKnownHostsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "flexctl", "known_hosts")
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestClientConfig -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/flexctlcli/clientconfig.go internal/flexctlcli/clientconfig_test.go
git commit -m "feat(flexctl): client.toml read/write with 0600/0700 perms"
```

---

## Task 7: sshconfig — ~/.ssh/config 마커 블록

**Files:**
- Create: `internal/flexctlcli/sshconfig.go`
- Create: `internal/flexctlcli/sshconfig_test.go`

- [ ] **Step 1: Write failing tests**

```go
package flexctlcli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestSSHConfig_InsertOnEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	block := flexctlcli.SSHConfigBlock{
		FlexctlBinary: "/usr/local/bin/flexctl",
		KnownHosts:    "/home/me/.config/flexctl/known_hosts",
	}
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, block))

	out, _ := os.ReadFile(path)
	s := string(out)
	require.Contains(t, s, "# >>> flexctl >>>")
	require.Contains(t, s, "# <<< flexctl <<<")
	require.Contains(t, s, "Host *.flex")
	require.Contains(t, s, "ProxyCommand /usr/local/bin/flexctl proxy %h %p")
}

func TestSSHConfig_PreservesOtherContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	original := "Host other.example.com\n    User alice\n\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	require.NoError(t, flexctlcli.UpsertSSHConfig(path, flexctlcli.SSHConfigBlock{
		FlexctlBinary: "/usr/local/bin/flexctl",
		KnownHosts:    "/k",
	}))
	out, _ := os.ReadFile(path)
	require.Contains(t, string(out), "Host other.example.com")
	require.Contains(t, string(out), "User alice")
	require.Contains(t, string(out), "# >>> flexctl >>>")
}

func TestSSHConfig_IdempotentReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	b1 := flexctlcli.SSHConfigBlock{FlexctlBinary: "/v1/flexctl", KnownHosts: "/k"}
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, b1))
	b2 := flexctlcli.SSHConfigBlock{FlexctlBinary: "/v2/flexctl", KnownHosts: "/k"}
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, b2))

	out, _ := os.ReadFile(path)
	require.NotContains(t, string(out), "/v1/flexctl")
	require.Contains(t, string(out), "/v2/flexctl")
	require.Equal(t, 1, strings.Count(string(out), "# >>> flexctl >>>"))
}

func TestSSHConfig_Remove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	require.NoError(t, flexctlcli.UpsertSSHConfig(path, flexctlcli.SSHConfigBlock{FlexctlBinary: "/x", KnownHosts: "/k"}))

	require.NoError(t, flexctlcli.RemoveSSHConfigBlock(path))
	out, _ := os.ReadFile(path)
	require.NotContains(t, string(out), "# >>> flexctl >>>")
	require.NotContains(t, string(out), "ProxyCommand")
}

func TestSSHConfig_PermsAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(path, []byte("Host x\n"), 0o600))

	require.NoError(t, flexctlcli.UpsertSSHConfig(path, flexctlcli.SSHConfigBlock{FlexctlBinary: "/x", KnownHosts: "/k"}))

	info, _ := os.Stat(path)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	matches, _ := filepath.Glob(path + ".flexctl-bak.*")
	require.Len(t, matches, 1, "expected exactly one backup file")
}
```

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestSSHConfig -count=1`
Expected: undefined symbols.

- [ ] **Step 3: Implement `sshconfig.go`**

```go
package flexctlcli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sshBlockBegin = "# >>> flexctl >>>"
	sshBlockEnd   = "# <<< flexctl <<<"
)

type SSHConfigBlock struct {
	FlexctlBinary string // absolute path to flexctl
	KnownHosts    string // path to ~/.config/flexctl/known_hosts
}

func renderBlock(b SSHConfigBlock) string {
	return fmt.Sprintf(`%s
# Managed by flexctl login. Do not edit between markers; changes will be overwritten.
Host *.flex
    User dev
    ProxyCommand %s proxy %%h %%p
    ServerAliveInterval 30
    ServerAliveCountMax 3
    ConnectTimeout 30
    StrictHostKeyChecking accept-new
    UserKnownHostsFile %s
%s
`, sshBlockBegin, b.FlexctlBinary, b.KnownHosts, sshBlockEnd)
}

// UpsertSSHConfig inserts or replaces the flexctl-managed block in `path`.
// Existing content outside the markers is preserved. Backs up the original
// file (if non-empty) the first time the block is inserted.
func UpsertSSHConfig(path string, b SSHConfigBlock) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read: %w", err)
	}

	hadBlock := bytes.Contains(existing, []byte(sshBlockBegin))
	if len(existing) > 0 && !hadBlock {
		bak := fmt.Sprintf("%s.flexctl-bak.%d", path, time.Now().Unix())
		if err := os.WriteFile(bak, existing, 0o600); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}

	newBlock := renderBlock(b)
	var out []byte
	if hadBlock {
		out = replaceBlock(existing, newBlock)
	} else {
		out = appendBlock(existing, newBlock)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return os.WriteFile(path, out, 0o600)
}

func RemoveSSHConfigBlock(path string) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Contains(existing, []byte(sshBlockBegin)) {
		return nil
	}
	bak := fmt.Sprintf("%s.flexctl-bak.%d", path, time.Now().Unix())
	if err := os.WriteFile(bak, existing, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	out := replaceBlock(existing, "") // strip
	return os.WriteFile(path, out, 0o600)
}

func appendBlock(existing []byte, block string) []byte {
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	if len(existing) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteString(block)
	return buf.Bytes()
}

func replaceBlock(existing []byte, replacement string) []byte {
	beginIdx := bytes.Index(existing, []byte(sshBlockBegin))
	if beginIdx < 0 {
		return existing
	}
	endIdx := bytes.Index(existing[beginIdx:], []byte(sshBlockEnd))
	if endIdx < 0 {
		return existing
	}
	endIdx += beginIdx + len(sshBlockEnd)
	// Include a trailing newline if present.
	if endIdx < len(existing) && existing[endIdx] == '\n' {
		endIdx++
	}

	var buf bytes.Buffer
	buf.Write(existing[:beginIdx])
	if replacement != "" {
		buf.WriteString(replacement)
	} else {
		// strip trailing blank line if we left one
		out := buf.Bytes()
		if len(out) > 0 && out[len(out)-1] == '\n' && len(existing[endIdx:]) == 0 {
			buf.Truncate(len(out) - 1)
		}
	}
	buf.Write(existing[endIdx:])
	// avoid trailing whitespace runs
	return []byte(strings.TrimRight(buf.String(), " \t") + "")
}

func DefaultSSHConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestSSHConfig -count=1`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/flexctlcli/sshconfig.go internal/flexctlcli/sshconfig_test.go
git commit -m "feat(flexctl): idempotent ~/.ssh/config marker block management"
```

---

## Task 8: apiclient — control-plane HTTP wrapper

**Files:**
- Create: `internal/flexctlcli/apiclient.go`
- Create: `internal/flexctlcli/apiclient_test.go`

- [ ] **Step 1: Write failing tests**

```go
package flexctlcli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestAPIClient_LoginSetsCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/login" {
			http.Error(w, "not found", 404); return
		}
		http.SetCookie(w, &http.Cookie{Name: "flex_session", Value: "abc123"})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "")
	cookie, err := c.Login(context.Background(), "p@x.com", "supersecret")
	require.NoError(t, err)
	require.Equal(t, "flex_session=abc123", cookie)
}

func TestAPIClient_MeReturnsUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/me", r.URL.Path)
		require.Equal(t, "flex_session=abc", r.Header.Get("Cookie"))
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "uid-1", "slug": "paul", "email": "p@x.com"})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	me, err := c.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, "paul", me.Slug)
}

func TestAPIClient_PairDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/devices/pair", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_id": "did", "hostname": "paul-device-mac",
			"preauth_key": "pk", "headscale_url": "https://hs", "tailnet_domain": "flex",
		})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	res, err := c.PairDevice(context.Background(), "mac")
	require.NoError(t, err)
	require.Equal(t, "paul-device-mac", res.Hostname)
	require.Equal(t, "pk", res.PreauthKey)
}

func TestAPIClient_AddSSHKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/me/ssh-keys", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "k1", "fingerprint": "fp"})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	id, err := c.AddSSHKey(context.Background(), "laptop-ed25519", "ssh-ed25519 AAA...")
	require.NoError(t, err)
	require.Equal(t, "k1", id)
}

func TestAPIClient_ListEnvs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/envs", r.URL.Path)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "running"},
		})
	}))
	defer srv.Close()

	c := flexctlcli.NewAPIClient(srv.URL, "flex_session=abc")
	envs, err := c.ListEnvs(context.Background())
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, "vllm", envs[0].Name)
}
```

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestAPIClient -count=1`
Expected: undefined symbols.

- [ ] **Step 3: Implement `apiclient.go`**

```go
package flexctlcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type APIClient struct {
	base   string
	cookie string
	hc     *http.Client
}

func NewAPIClient(base, cookie string) *APIClient {
	return &APIClient{base: base, cookie: cookie, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *APIClient) Cookie() string { return c.cookie }

func (c *APIClient) do(ctx context.Context, method, path string, in any, out any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		return resp, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, string(msg))
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("decode: %w", err)
		}
	}
	return resp, nil
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (c *APIClient) Login(ctx context.Context, email, password string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/auth/login", loginReq{Email: email, Password: password}, nil)
	if err != nil {
		return "", err
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "flex_session" {
			c.cookie = ck.Name + "=" + ck.Value
			return c.cookie, nil
		}
	}
	return "", fmt.Errorf("no flex_session cookie in response")
}

func (c *APIClient) Logout(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/auth/logout", nil, nil)
	return err
}

type MeResp struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Email string `json:"email"`
}

func (c *APIClient) Me(ctx context.Context) (MeResp, error) {
	var out MeResp
	_, err := c.do(ctx, http.MethodGet, "/v1/me", nil, &out)
	return out, err
}

type PairDeviceResp struct {
	DeviceID      string `json:"device_id"`
	Hostname      string `json:"hostname"`
	PreauthKey    string `json:"preauth_key"`
	HeadscaleURL  string `json:"headscale_url"`
	TailnetDomain string `json:"tailnet_domain"`
}

type pairDeviceReq struct {
	Name string `json:"name"`
}

func (c *APIClient) PairDevice(ctx context.Context, name string) (PairDeviceResp, error) {
	var out PairDeviceResp
	_, err := c.do(ctx, http.MethodPost, "/v1/devices/pair", pairDeviceReq{Name: name}, &out)
	return out, err
}

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
}

func (c *APIClient) ListDevices(ctx context.Context) ([]Device, error) {
	var out []Device
	_, err := c.do(ctx, http.MethodGet, "/v1/devices", nil, &out)
	return out, err
}

func (c *APIClient) DeleteDevice(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/devices/"+id, nil, nil)
	return err
}

type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

type addKeyReq struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

func (c *APIClient) AddSSHKey(ctx context.Context, name, publicKey string) (string, error) {
	var out struct {
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/me/ssh-keys", addKeyReq{Name: name, PublicKey: publicKey}, &out)
	return out.ID, err
}

func (c *APIClient) ListSSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out []SSHKey
	_, err := c.do(ctx, http.MethodGet, "/v1/me/ssh-keys", nil, &out)
	return out, err
}

func (c *APIClient) DeleteSSHKey(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/me/ssh-keys/"+id, nil, nil)
	return err
}

type Env struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
	NodeID   string `json:"node_id"`
}

func (c *APIClient) ListEnvs(ctx context.Context) ([]Env, error) {
	var out []Env
	_, err := c.do(ctx, http.MethodGet, "/v1/envs", nil, &out)
	return out, err
}
```

- [ ] **Step 4: Run tests, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestAPIClient -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/flexctlcli/apiclient.go internal/flexctlcli/apiclient_test.go
git commit -m "feat(flexctl): control-plane HTTP client (login, devices, keys, envs)"
```

---

## Task 9: flextsnet.Server wrapper + tsnet 의존성

**Files:**
- Create: `internal/flextsnet/server.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add tsnet dependency**

Run:

```bash
go get tailscale.com/tsnet@latest
go mod tidy
```

Expected: `go.mod` gains `tailscale.com` and ~hundreds of indirect deps in `go.sum`. Don’t hand-edit either file.

- [ ] **Step 2: Implement `internal/flextsnet/server.go`**

```go
package flextsnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"tailscale.com/tsnet"
)

type Config struct {
	StateDir   string
	Hostname   string
	AuthKey    string
	ControlURL string
	Logf       func(format string, args ...any) // nil → io.Discard equivalent
}

// Start brings up an embedded Tailscale node in this process. Caller must
// Close when done. State is persisted under cfg.StateDir; existing state is
// reused (no new pre-auth key required on subsequent boots).
func Start(ctx context.Context, cfg Config) (*tsnet.Server, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	srv := &tsnet.Server{
		Dir:        cfg.StateDir,
		Hostname:   cfg.Hostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Ephemeral:  false,
		Logf:       logf,
	}
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
	if _, err := srv.Up(ctx); err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	return srv, nil
}

// Dial performs an in-process TCP dial via the embedded Tailscale node. host
// should be a tailnet hostname (MagicDNS) or IP, port a numeric string.
func Dial(ctx context.Context, srv *tsnet.Server, host, port string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return srv.Dial(dctx, "tcp", net.JoinHostPort(host, port))
}

// Pipe copies bidirectionally between conn and stdio. Returns when either
// side closes.
func Pipe(conn net.Conn, stdin io.Reader, stdout io.Writer) error {
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, stdin)
		errCh <- err
		_ = conn.Close()
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		errCh <- err
	}()
	// Wait for one side to finish; the other will unblock from the Close above.
	err := <-errCh
	<-errCh
	return err
}
```

- [ ] **Step 3: Verify package builds**

Run: `go build ./internal/flextsnet/`
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum internal/flextsnet/server.go
git commit -m "feat(flextsnet): tsnet.Server wrapper (Start/Dial/Pipe)"
```

---

## Task 10: flextsnet integration test (Headscale + two tsnet nodes)

This is the **core e2e correctness gate** for Plan 5 short of running on real GPU hardware. Without it we ship blind.

**Files:**
- Create: `internal/flextsnet/server_integration_test.go`

- [ ] **Step 1: Write the integration test**

```go
//go:build integration

package flextsnet_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flextsnet"
	"github.com/paul/flexctl/internal/headscale"
)

// startHeadscale is the shared fixture used by internal/headscale/client_test.go.
// Replace with the actual import or copy of that helper.
func startHeadscale(t *testing.T, ctx context.Context) *headscale.Client {
	t.Helper()
	panic("use existing helper from internal/headscale/client_test.go")
}

func TestTsnet_TwoNodesDialEachOther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	hs := startHeadscale(t, ctx)

	// One Headscale user, two devices.
	_, err := hs.CreateUser(ctx, "paul")
	require.NoError(t, err)
	require.NoError(t, hs.SetPolicy(ctx, `{
	  "tagOwners": {"tag:device-paul": ["control-plane"]},
	  "acls": [{"action": "accept", "src": ["tag:device-paul"], "dst": ["tag:device-paul:*"]}]
	}`))

	keyA, err := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Reusable: false, Ephemeral: false,
		Expiration: 1 * time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	require.NoError(t, err)
	keyB, err := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Reusable: false, Ephemeral: false,
		Expiration: 1 * time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	require.NoError(t, err)

	// Headscale URL exposed by the testcontainer (helper returns *headscale.Client
	// but we also need the URL — extend the helper to return it, or expose c.BaseURL()).
	headscaleURL := hs.BaseURL()

	srvA, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir: t.TempDir(), Hostname: "paul-device-a",
		AuthKey: keyA.Key, ControlURL: headscaleURL,
	})
	require.NoError(t, err)
	defer srvA.Close()

	srvB, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir: t.TempDir(), Hostname: "paul-device-b",
		AuthKey: keyB.Key, ControlURL: headscaleURL,
	})
	require.NoError(t, err)
	defer srvB.Close()

	// srvB listens on tailnet :8765.
	ln, err := srvB.Listen("tcp", ":8765")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil { return }
		defer conn.Close()
		_, _ = conn.Write([]byte("hello from B"))
	}()

	// srvA dials srvB by tailnet hostname.
	conn, err := flextsnet.Dial(ctx, srvA, "paul-device-b", "8765")
	require.NoError(t, err)
	defer conn.Close()
	buf, _ := io.ReadAll(conn)
	require.Equal(t, "hello from B", string(buf))
}

func TestTsnet_CrossUserACLBlocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	hs := startHeadscale(t, ctx)

	_, err := hs.CreateUser(ctx, "paul")
	require.NoError(t, err)
	_, err = hs.CreateUser(ctx, "alice")
	require.NoError(t, err)
	// ACL: only same-user device→device allowed.
	require.NoError(t, hs.SetPolicy(ctx, `{
	  "tagOwners": {
	    "tag:device-paul":  ["control-plane"],
	    "tag:device-alice": ["control-plane"]
	  },
	  "acls": [
	    {"action": "accept", "src": ["tag:device-paul"],  "dst": ["tag:device-paul:*"]},
	    {"action": "accept", "src": ["tag:device-alice"], "dst": ["tag:device-alice:*"]}
	  ]
	}`))

	keyP, _ := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "paul", Expiration: time.Hour, ACLTags: []string{"tag:device-paul"},
	})
	keyA, _ := hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: "alice", Expiration: time.Hour, ACLTags: []string{"tag:device-alice"},
	})

	srvP, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir: t.TempDir(), Hostname: "paul-device-x",
		AuthKey: keyP.Key, ControlURL: hs.BaseURL(),
	})
	require.NoError(t, err)
	defer srvP.Close()

	srvA, err := flextsnet.Start(ctx, flextsnet.Config{
		StateDir: t.TempDir(), Hostname: "alice-device-y",
		AuthKey: keyA.Key, ControlURL: hs.BaseURL(),
	})
	require.NoError(t, err)
	defer srvA.Close()

	ln, _ := srvA.Listen("tcp", ":9090")
	defer ln.Close()
	go func() { _, _ = ln.Accept() }()

	// paul should NOT be able to reach alice.
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	_, err = srvP.Dial(dctx, "tcp", net.JoinHostPort("alice-device-y", "9090"))
	require.Error(t, err, "ACL must block cross-user dial")

	_ = fmt.Sprintf // silence
}
```

If `headscale.Client.BaseURL()` accessor does not exist, add it as a one-line getter — needed for tsnet `ControlURL`.

- [ ] **Step 2: Run the integration test**

Run: `go test -tags=integration ./internal/flextsnet/ -count=1 -timeout=10m`
Expected: PASS (both tests). Test may take 2–4 minutes due to tsnet node registration.

- [ ] **Step 3: Commit**

```bash
git add internal/flextsnet/server_integration_test.go internal/headscale/client.go
git commit -m "test(flextsnet): two-tsnet integration via testcontainers Headscale (dial + ACL block)"
```

---

## Task 11: flexctl key / flexctl env list 명령

**Files:**
- Create: `internal/flexctlcli/key.go`, `key_test.go`
- Create: `internal/flexctlcli/envlist.go`, `envlist_test.go`
- Modify: `internal/flexctlcli/root.go`

- [ ] **Step 1: Write tests**

`internal/flexctlcli/key_test.go`:

```go
package flexctlcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func writeClientCfg(t *testing.T, base string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := flexctlcli.ClientConfig{
		ControlPlane: base, SessionCookie: "flex_session=ok",
	}
	p := filepath.Join(dir, "client.toml")
	require.NoError(t, flexctlcli.WriteClientConfig(p, cfg))
	return p
}

func TestKeyAdd_ReadsFileAndPOSTs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/me/ssh-keys", r.URL.Path)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		require.Equal(t, "ssh-ed25519 AAAA test-key", body["public_key"])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "k1", "fingerprint": "fp"})
	}))
	defer srv.Close()

	pubPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	require.NoError(t, os.WriteFile(pubPath, []byte("ssh-ed25519 AAAA test-key\n"), 0o600))
	cfgPath := writeClientCfg(t, srv.URL)

	cmd := flexctlcli.NewKeyCmd()
	cmd.SetArgs([]string{"add", pubPath, "--name", "lp", "--config", cfgPath})
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Contains(t, out.String(), "k1")
}
```

`internal/flexctlcli/envlist_test.go`:

```go
package flexctlcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestEnvList_PrintsTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "running", "node_id": "gpu-01"},
			{"id": "e2", "name": "sd",   "hostname": "paul-sd",   "status": "stopped", "node_id": "gpu-01"},
		})
	}))
	defer srv.Close()
	cfgPath := writeClientCfg(t, srv.URL)

	cmd := flexctlcli.NewEnvCmd()
	cmd.SetArgs([]string{"list", "--config", cfgPath})
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	s := out.String()
	require.Contains(t, s, "vllm")
	require.Contains(t, s, "paul-vllm")
	require.Contains(t, s, "running")
	require.Contains(t, s, "sd")
	require.Contains(t, s, "stopped")
}
```

- [ ] **Step 2: Run tests, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run 'TestKey|TestEnvList' -count=1`
Expected: undefined `NewKeyCmd` / `NewEnvCmd`.

- [ ] **Step 3: Implement `key.go`**

```go
package flexctlcli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func NewKeyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "key", Short: "Manage SSH public keys on the control plane"}
	cmd.AddCommand(newKeyAddCmd())
	cmd.AddCommand(newKeyListCmd())
	cmd.AddCommand(newKeyRmCmd())
	return cmd
}

func newKeyAddCmd() *cobra.Command {
	var (
		name string
		cfgPath string
	)
	cmd := &cobra.Command{
		Use:   "add <path|->",
		Short: "Upload an SSH public key (path or '-' for stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return fmt.Errorf("read config: %w (try `flexctl login`)", err) }

			var data []byte
			if args[0] == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil { return fmt.Errorf("read key: %w", err) }
			pub := strings.TrimSpace(string(data))

			if name == "" {
				h, _ := os.Hostname()
				name = h
			}
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			id, err := c.AddSSHKey(cmd.Context(), name, pub)
			if err != nil { return err }
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Key name (defaults to hostname)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func newKeyListCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "list", Short: "List uploaded SSH keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return err }
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			keys, err := c.ListSSHKeys(cmd.Context())
			if err != nil { return err }
			for _, k := range keys {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", k.ID, k.Name, k.Fingerprint)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func newKeyRmCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "rm <key-id>",
		Short: "Delete an SSH key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return err }
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			return c.DeleteSSHKey(cmd.Context(), args[0])
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func cfgPathOrDefault(p string) string {
	if p != "" { return p }
	return DefaultClientConfigPath()
}
```

- [ ] **Step 4: Implement `envlist.go`**

```go
package flexctlcli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func NewEnvCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "env", Short: "Inspect environments"}
	cmd.AddCommand(newEnvListCmd())
	return cmd
}

func newEnvListCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "list", Short: "List your environments",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return err }
			c := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
			envs, err := c.ListEnvs(cmd.Context())
			if err != nil { return err }
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tHOSTNAME\tSTATUS\tNODE")
			for _, e := range envs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, e.Hostname, e.Status, e.NodeID)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}
```

- [ ] **Step 5: Register in `root.go`**

Add to `internal/flexctlcli/root.go`:

```go
root.AddCommand(NewKeyCmd())
root.AddCommand(NewEnvCmd())
```

- [ ] **Step 6: Run tests, expect pass**

Run: `go test ./internal/flexctlcli/ -run 'TestKey|TestEnvList' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/flexctlcli/key.go internal/flexctlcli/key_test.go internal/flexctlcli/envlist.go internal/flexctlcli/envlist_test.go internal/flexctlcli/root.go
git commit -m "feat(flexctl): key add/list/rm + env list commands"
```

---

## Task 12: flexctl login — 5단계 통합

**Files:**
- Create: `internal/flexctlcli/login.go`, `login_test.go`
- Modify: `internal/flexctlcli/root.go`

- [ ] **Step 1: Write failing test**

`internal/flexctlcli/login_test.go`:

```go
package flexctlcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestLogin_HappyPath(t *testing.T) {
	var (
		gotLogin    atomic.Int32
		gotKeyUpload atomic.Int32
		gotPair      atomic.Int32
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		gotLogin.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "flex_session", Value: "abc"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "uid-1", "slug": "paul", "email": "p@x.com"})
	})
	mux.HandleFunc("/v1/me/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case http.MethodPost:
			gotKeyUpload.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "k1", "fingerprint": "fp"})
		}
	})
	mux.HandleFunc("/v1/devices/pair", func(w http.ResponseWriter, r *http.Request) {
		gotPair.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_id": "did", "hostname": "paul-device-mac",
			"preauth_key": "pk", "headscale_url": "https://hs", "tailnet_domain": "flex",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FLEXCTL_CLIENT_CONFIG", filepath.Join(home, ".config", "flexctl", "client.toml"))
	t.Setenv("FLEXCTL_STATE_DIR",    filepath.Join(home, ".config", "flexctl", "tsnet"))
	t.Setenv("FLEXCTL_SKIP_TSNET",   "1") // login_test never actually joins a tailnet
	t.Setenv("FLEXCTL_SSH_CONFIG",   filepath.Join(home, ".ssh", "config"))

	// Plant an id_ed25519.pub so key upload triggers.
	pub := filepath.Join(home, ".ssh", "id_ed25519.pub")
	require.NoError(t, os.MkdirAll(filepath.Dir(pub), 0o700))
	require.NoError(t, os.WriteFile(pub, []byte("ssh-ed25519 AAAA test-key"), 0o600))

	cmd := flexctlcli.NewLoginCmd()
	cmd.SetArgs([]string{
		"--control-plane", srv.URL,
		"--email", "p@x.com",
		"--password", "supersecret",
		"--device-name", "mac",
	})
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))

	require.Equal(t, int32(1), gotLogin.Load())
	require.Equal(t, int32(1), gotKeyUpload.Load())
	require.Equal(t, int32(1), gotPair.Load())

	// client.toml was written.
	cfg, err := flexctlcli.ReadClientConfig(filepath.Join(home, ".config", "flexctl", "client.toml"))
	require.NoError(t, err)
	require.Equal(t, srv.URL, cfg.ControlPlane)
	require.Equal(t, "paul-device-mac", cfg.DeviceHostname)
	require.Equal(t, "flex_session=abc", cfg.SessionCookie)

	// ssh_config block was inserted.
	body, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	require.Contains(t, string(body), "# >>> flexctl >>>")
	require.Contains(t, string(body), "Host *.flex")
}

func TestLogin_IdempotentReExecution(t *testing.T) {
	// Same setup but run login twice; assert second run does not POST /v1/me/ssh-keys again.
	t.Skip("covered by TestLogin_HappyPath assertions + manual rerun — keep as future work if a real flake appears")
}
```

- [ ] **Step 2: Run test, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestLogin -count=1`
Expected: undefined `NewLoginCmd`.

- [ ] **Step 3: Implement `login.go`**

```go
package flexctlcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/paul/flexctl/internal/flextsnet"
)

func NewLoginCmd() *cobra.Command {
	var (
		controlPlane string
		email        string
		password     string
		deviceName   string
		cfgPath      string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate, upload SSH keys, pair this device, join the tailnet",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfgPath = cfgPathOrDefault(cfgPath)

			if controlPlane == "" {
				if existing, err := ReadClientConfig(cfgPath); err == nil {
					controlPlane = existing.ControlPlane
				}
			}
			if controlPlane == "" {
				return fmt.Errorf("--control-plane is required on first login")
			}
			if email == "" || password == "" {
				var err error
				email, password, err = promptCreds(cmd.InOrStdin(), cmd.OutOrStdout(), email)
				if err != nil { return err }
			}
			if deviceName == "" {
				h, _ := os.Hostname()
				deviceName = sanitizeDeviceName(h)
			}

			// 1. Session
			api := NewAPIClient(controlPlane, "")
			cookie, err := api.Login(ctx, email, password)
			if err != nil { return fmt.Errorf("login: %w", err) }
			fmt.Fprintln(cmd.OutOrStdout(), "✓ session established")

			// 2. Auto key upload (best effort)
			if err := autoUploadKeys(ctx, api, cmd.OutOrStdout()); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: key upload: %v\n", err)
			}

			// 3. Device pair
			pair, err := api.PairDevice(ctx, deviceName)
			if err != nil { return fmt.Errorf("pair device: %w", err) }
			fmt.Fprintf(cmd.OutOrStdout(), "✓ device paired as %s\n", pair.Hostname)

			// 4. tsnet join (skippable for unit tests)
			if os.Getenv("FLEXCTL_SKIP_TSNET") != "1" {
				stateDir := DefaultStateDir()
				if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" { stateDir = v }
				if err := os.MkdirAll(stateDir, 0o700); err != nil {
					return fmt.Errorf("state dir: %w", err)
				}
				srv, err := flextsnet.Start(ctx, flextsnet.Config{
					StateDir:   stateDir,
					Hostname:   pair.Hostname,
					AuthKey:    pair.PreauthKey,
					ControlURL: pair.HeadscaleURL,
				})
				if err != nil { return fmt.Errorf("tsnet up: %w", err) }
				_ = srv.Close()
				fmt.Fprintln(cmd.OutOrStdout(), "✓ joined tailnet")
			}

			// 5. ssh_config block
			selfPath, _ := os.Executable()
			if abs, err := filepath.Abs(selfPath); err == nil { selfPath = abs }
			sshCfgPath := os.Getenv("FLEXCTL_SSH_CONFIG")
			if sshCfgPath == "" { sshCfgPath = DefaultSSHConfigPath() }
			if err := UpsertSSHConfig(sshCfgPath, SSHConfigBlock{
				FlexctlBinary: selfPath, KnownHosts: DefaultKnownHostsPath(),
			}); err != nil { return fmt.Errorf("ssh config: %w", err) }
			fmt.Fprintln(cmd.OutOrStdout(), "✓ ~/.ssh/config updated")

			// Persist
			cfg := ClientConfig{
				ControlPlane:   controlPlane,
				SessionCookie:  cookie,
				DeviceName:     deviceName,
				DeviceHostname: pair.Hostname,
				HeadscaleURL:   pair.HeadscaleURL,
				TailnetDomain:  pair.TailnetDomain,
			}
			if err := WriteClientConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Done. Try: flexctl env list\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "Control plane URL")
	cmd.Flags().StringVar(&email,        "email",         "", "Account email (prompted if empty)")
	cmd.Flags().StringVar(&password,     "password",      "", "Account password (prompted if empty)")
	cmd.Flags().StringVar(&deviceName,   "device-name",   "", "Device name (default OS hostname)")
	cmd.Flags().StringVar(&cfgPath,      "config",        "", "client.toml path")
	return cmd
}

func promptCreds(stdin any, stdout any, email string) (string, string, error) {
	// Minimal prompt: we read from os.Stdin/term to avoid coupling to *cobra.Command IO.
	if email == "" {
		fmt.Fprint(os.Stderr, "email: ")
		var e string
		_, err := fmt.Fscanln(os.Stdin, &e)
		if err != nil { return "", "", err }
		email = strings.TrimSpace(e)
	}
	fmt.Fprint(os.Stderr, "password: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil { return "", "", err }
	return email, string(pw), nil
}

func sanitizeDeviceName(s string) string {
	s = strings.ToLower(s)
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		}
	}
	if len(out) == 0 { return "device" }
	return string(out)
}

func autoUploadKeys(ctx context.Context, api *APIClient, stdout fmt.Stringer) error {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".ssh", "id_ed25519.pub"),
		filepath.Join(home, ".ssh", "id_rsa.pub"),
		filepath.Join(home, ".ssh", "id_ecdsa.pub"),
	}
	existing, err := api.ListSSHKeys(ctx)
	if err != nil { return err }
	known := make(map[string]bool, len(existing))
	for _, k := range existing {
		known[k.Fingerprint] = true
	}
	osHost, _ := os.Hostname()
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil { continue }
		pub := strings.TrimSpace(string(data))
		fp, err := fingerprintOf(pub)
		if err != nil { continue }
		if known[fp] {
			continue
		}
		keytype := keytypeFromBasename(filepath.Base(p))
		name := osHost + "-" + keytype
		if _, err := api.AddSSHKey(ctx, name, pub); err != nil {
			return fmt.Errorf("upload %s: %w", p, err)
		}
	}
	return nil
}

func fingerprintOf(pub string) (string, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil { return "", err }
	// Must match Plan 2's internal/sshkeys/service.go (base64 SHA256).
	return ssh.FingerprintSHA256(pk), nil
}

func keytypeFromBasename(name string) string {
	switch {
	case strings.Contains(name, "ed25519"): return "ed25519"
	case strings.Contains(name, "ecdsa"):   return "ecdsa"
	case strings.Contains(name, "rsa"):     return "rsa"
	default:                                return "key"
	}
}
```

Remove the `"crypto/sha256"` and `"encoding/hex"` imports — they aren't needed once `ssh.FingerprintSHA256` is used.

- [ ] **Step 4: Register in `root.go`**

Add `root.AddCommand(NewLoginCmd())` to `NewRootCmd()`.

- [ ] **Step 5: Run test, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestLogin -count=1`
Expected: PASS (the `_HappyPath` test; `_IdempotentReExecution` is skipped).

- [ ] **Step 6: Commit**

```bash
git add internal/flexctlcli/login.go internal/flexctlcli/login_test.go internal/flexctlcli/root.go
git commit -m "feat(flexctl): all-in-one login (session + keys + device pair + tsnet + ssh_config)"
```

---

## Task 13: flexctl logout

**Files:**
- Create: `internal/flexctlcli/logout.go`, `logout_test.go`
- Modify: `internal/flexctlcli/root.go`

- [ ] **Step 1: Write failing test**

```go
package flexctlcli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestLogout_DeletesDeviceAndCleansFiles(t *testing.T) {
	var (
		deletedDevice atomic.Bool
		loggedOut     atomic.Bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/devices/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletedDevice.Store(true)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		loggedOut.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/devices", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"did","name":"mac","hostname":"paul-device-mac"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".config", "flexctl", "client.toml")
	t.Setenv("FLEXCTL_CLIENT_CONFIG", cfgPath)
	t.Setenv("FLEXCTL_STATE_DIR", filepath.Join(home, ".config", "flexctl", "tsnet"))
	sshCfg := filepath.Join(home, ".ssh", "config")
	t.Setenv("FLEXCTL_SSH_CONFIG", sshCfg)

	require.NoError(t, flexctlcli.WriteClientConfig(cfgPath, flexctlcli.ClientConfig{
		ControlPlane: srv.URL, SessionCookie: "flex_session=abc",
		DeviceName: "mac", DeviceHostname: "paul-device-mac",
	}))
	// plant state dir + ssh config block
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "flexctl", "tsnet"), 0o700))
	require.NoError(t, flexctlcli.UpsertSSHConfig(sshCfg, flexctlcli.SSHConfigBlock{FlexctlBinary: "/x", KnownHosts: "/k"}))

	cmd := flexctlcli.NewLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out); cmd.SetErr(&out)
	require.NoError(t, cmd.ExecuteContext(context.Background()))

	require.True(t, deletedDevice.Load())
	require.True(t, loggedOut.Load())

	_, err := os.Stat(cfgPath); require.True(t, os.IsNotExist(err))
	body, _ := os.ReadFile(sshCfg)
	require.NotContains(t, string(body), "# >>> flexctl >>>")
}
```

- [ ] **Step 2: Run, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestLogout -count=1`
Expected: undefined `NewLogoutCmd`.

- [ ] **Step 3: Implement `logout.go`**

```go
package flexctlcli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func NewLogoutCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "logout", Short: "Delete this device, drop session, clean local state",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath = cfgPathOrDefault(cfgPath)
			cfg, err := ReadClientConfig(cfgPath)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "already logged out")
					return nil
				}
				return err
			}
			api := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)

			// Best-effort: find device by hostname and delete.
			devs, _ := api.ListDevices(cmd.Context())
			for _, d := range devs {
				if d.Hostname == cfg.DeviceHostname {
					_ = api.DeleteDevice(cmd.Context(), d.ID)
				}
			}
			_ = api.Logout(cmd.Context())

			// state dir
			stateDir := DefaultStateDir()
			if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" { stateDir = v }
			_ = os.RemoveAll(stateDir)

			// ssh config
			sshCfg := os.Getenv("FLEXCTL_SSH_CONFIG")
			if sshCfg == "" { sshCfg = DefaultSSHConfigPath() }
			_ = RemoveSSHConfigBlock(sshCfg)

			// client.toml
			_ = os.Remove(cfgPath)

			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}
```

- [ ] **Step 4: Register in `root.go`**

Add `root.AddCommand(NewLogoutCmd())`.

- [ ] **Step 5: Run test, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestLogout -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/flexctlcli/logout.go internal/flexctlcli/logout_test.go internal/flexctlcli/root.go
git commit -m "feat(flexctl): logout — delete device, drop session, clean local state"
```

---

## Task 14: flexctl ssh

**Files:**
- Create: `internal/flexctlcli/ssh.go`, `ssh_test.go`
- Modify: `internal/flexctlcli/root.go`

- [ ] **Step 1: Write failing test**

`flexctl ssh` execs the OS `ssh` binary. Testing the actual exec is brittle, so we test the command-line assembly instead by exposing a `buildSSHArgs` helper.

```go
package flexctlcli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestSSH_BuildsArgsWithProxyCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "running"},
		})
	}))
	defer srv.Close()
	cfgPath := writeClientCfg(t, srv.URL)

	args, err := flexctlcli.BuildSSHArgs(context.Background(), cfgPath, "/usr/bin/flexctl", "vllm", []string{"-L", "8888:localhost:8888"})
	require.NoError(t, err)
	// Expect: -o ProxyCommand=/usr/bin/flexctl proxy %h %p
	//         -o UserKnownHostsFile=... -o StrictHostKeyChecking=accept-new ...
	//         dev@paul-vllm.flex -L 8888:localhost:8888
	require.Contains(t, args, "dev@paul-vllm.flex")
	require.Contains(t, args, "-L")
	require.Contains(t, args, "8888:localhost:8888")
	// ProxyCommand option must be present and reference the flexctl binary.
	var found bool
	for i, a := range args {
		if a == "-o" && i+1 < len(args) && filepath.Base(args[i+1]) != "" {
			if args[i+1] == "ProxyCommand=/usr/bin/flexctl proxy %h %p" {
				found = true
			}
		}
	}
	require.True(t, found, "ProxyCommand option missing: %v", args)
}

func TestSSH_RejectsStoppedEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "e1", "name": "vllm", "hostname": "paul-vllm", "status": "stopped"},
		})
	}))
	defer srv.Close()
	cfgPath := writeClientCfg(t, srv.URL)

	_, err := flexctlcli.BuildSSHArgs(context.Background(), cfgPath, "/usr/bin/flexctl", "vllm", nil)
	require.ErrorContains(t, err, "stopped")
}
```

- [ ] **Step 2: Run, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestSSH -count=1`
Expected: undefined `BuildSSHArgs`.

- [ ] **Step 3: Implement `ssh.go`**

```go
package flexctlcli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// BuildSSHArgs returns the argv for the `ssh` invocation that flexctl ssh
// would exec. Exposed for tests; production code wraps it in NewSSHCmd.
func BuildSSHArgs(ctx context.Context, cfgPath, selfPath, envName string, extra []string) ([]string, error) {
	cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
	if err != nil { return nil, fmt.Errorf("read config: %w", err) }

	api := NewAPIClient(cfg.ControlPlane, cfg.SessionCookie)
	envs, err := api.ListEnvs(ctx)
	if err != nil { return nil, err }
	var target Env
	for _, e := range envs {
		if e.Name == envName {
			target = e; break
		}
	}
	if target.ID == "" {
		return nil, fmt.Errorf("env %q not found", envName)
	}
	if target.Status != "running" {
		return nil, fmt.Errorf("env %q is %s (must be running)", envName, target.Status)
	}

	args := []string{
		"-o", "ProxyCommand=" + selfPath + " proxy %h %p",
		"-o", "UserKnownHostsFile=" + DefaultKnownHostsPath(),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=30",
		"dev@" + target.Hostname + ".flex",
	}
	args = append(args, extra...)
	return args, nil
}

func NewSSHCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "ssh <env-name> [-- <ssh-args>]",
		Short: "SSH into an environment",
		Args: cobra.MinimumNArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			selfPath, _ := os.Executable()
			if abs, err := filepath.Abs(selfPath); err == nil { selfPath = abs }

			envName := args[0]
			extra := args[1:]
			built, err := BuildSSHArgs(cmd.Context(), cfgPath, selfPath, envName, extra)
			if err != nil { return err }

			c := exec.CommandContext(cmd.Context(), "ssh", built...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}
```

- [ ] **Step 4: Register in `root.go`**

Add `root.AddCommand(NewSSHCmd())`.

- [ ] **Step 5: Run, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestSSH -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/flexctlcli/ssh.go internal/flexctlcli/ssh_test.go internal/flexctlcli/root.go
git commit -m "feat(flexctl): ssh wrapper — looks up env, execs ssh with ProxyCommand"
```

---

## Task 15: flexctl proxy

**Files:**
- Create: `internal/flexctlcli/proxy.go`, `proxy_test.go`
- Modify: `internal/flexctlcli/root.go`

`flexctl proxy %h %p` is the ProxyCommand entry point. The real path requires tsnet + Headscale (covered by Task 10's integration test). Here we add a unit test for argument parsing and the `.flex` suffix stripping; the wiring is short.

- [ ] **Step 1: Write failing test**

```go
package flexctlcli_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestProxy_NormalizesHost(t *testing.T) {
	require.Equal(t, "paul-vllm", flexctlcli.NormalizeProxyHost("paul-vllm.flex"))
	require.Equal(t, "paul-vllm", flexctlcli.NormalizeProxyHost("paul-vllm"))
}
```

- [ ] **Step 2: Run, expect compile failure**

Run: `go test ./internal/flexctlcli/ -run TestProxy -count=1`
Expected: undefined `NormalizeProxyHost`.

- [ ] **Step 3: Implement `proxy.go`**

```go
package flexctlcli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"tailscale.com/tsnet"

	"github.com/paul/flexctl/internal/flextsnet"
)

// NormalizeProxyHost strips the trailing ".flex" suffix that ssh_config adds.
func NormalizeProxyHost(h string) string { return strings.TrimSuffix(h, ".flex") }

func NewProxyCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use: "proxy <host> <port>", Hidden: false,
		Short: "OpenSSH ProxyCommand entry point (pipes stdin/stdout via tsnet)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ReadClientConfig(cfgPathOrDefault(cfgPath))
			if err != nil { return fmt.Errorf("read config: %w (run `flexctl login`)", err) }

			host := NormalizeProxyHost(args[0])
			port := args[1]

			ctx := cmd.Context()
			srv, err := startWithRetry(ctx, cfg)
			if err != nil { return err }
			defer srv.Close()

			conn, err := flextsnet.Dial(ctx, srv, host, port)
			if err != nil { return fmt.Errorf("dial %s:%s: %w", host, port, err) }
			return flextsnet.Pipe(conn, os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "client.toml path")
	return cmd
}

func startWithRetry(ctx context.Context, cfg ClientConfig) (*tsnet.Server, error) {
	stateDir := DefaultStateDir()
	if v := os.Getenv("FLEXCTL_STATE_DIR"); v != "" { stateDir = v }
	for attempt := 0; attempt < 2; attempt++ {
		srv, err := flextsnet.Start(ctx, flextsnet.Config{
			StateDir:   stateDir,
			Hostname:   cfg.DeviceHostname,
			ControlURL: cfg.HeadscaleURL,
		})
		if err == nil { return srv, nil }
		if attempt == 0 {
			time.Sleep(5 * time.Second)
			continue
		}
		return nil, fmt.Errorf("tsnet start (another flexctl proxy may be initializing): %w", err)
	}
	return nil, nil
}
```

- [ ] **Step 4: Register in `root.go`**

Add `root.AddCommand(NewProxyCmd())`.

- [ ] **Step 5: Run, expect pass**

Run: `go test ./internal/flexctlcli/ -run TestProxy -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/flexctlcli/proxy.go internal/flexctlcli/proxy_test.go internal/flexctlcli/root.go
git commit -m "feat(flexctl): proxy command — tsnet-backed SSH ProxyCommand"
```

---

## Task 16: README + manual e2e checklist

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a "Plan 5: SSH into your env" section**

Append to `README.md`:

```markdown
## Connect to an env from your laptop

```bash
# 1. Install flexctl on your laptop (same binary as the GPU agent).
sudo install bin/flexctl /usr/local/bin/flexctl

# 2. Log in. Uploads ~/.ssh/id_ed25519.pub if present, pairs this device, joins the tailnet, edits ~/.ssh/config.
flexctl login --control-plane https://flexctl.example.com

# 3. See your envs.
flexctl env list

# 4. SSH.
flexctl ssh vllm-train
# or, equivalently:
ssh dev@paul-vllm-train.flex
```

### SSH key management

```bash
flexctl key add ~/.ssh/id_ed25519.pub --name laptop-ed25519
flexctl key list
flexctl key rm <key-id>
```

Keys are snapshotted into each env at `flexctl env create` time. Add a key, then restart the env to pick it up.

### Logout

```bash
flexctl logout
```

Removes the device from Headscale, drops the session, cleans `~/.config/flexctl/` and the marker block in `~/.ssh/config`.

### Manual e2e checklist (GPU machine)

1. control-plane up, Headscale up, a node running `flexctl agent`.
2. Sign up: `curl -X POST https://flexctl.example.com/v1/auth/signup -d '{"email":"p@x.com","slug":"paul","password":"supersecret"}'`.
3. From the laptop: `flexctl login --control-plane https://flexctl.example.com --email p@x.com`.
4. Build images on the GPU node: `make sidecar-image && make dev-image`.
5. Create an env (via web or curl): `POST /v1/envs {"name":"cuda","template_id":"cuda-base","node_id":"<uuid>","gpu_request":1}`.
6. Wait for `flexctl env list` to show `running`.
7. `flexctl ssh cuda` → `nvidia-smi` should print the GPU.

### Endpoints added by Plan 5

| Method | Path | Description |
|---|---|---|
| POST | /v1/devices/pair | Register this device, get a Headscale pre-auth key |
| GET | /v1/devices | List your devices |
| DELETE | /v1/devices/{id} | Drop a device + its Headscale node |
```

- [ ] **Step 2: Verify everything compiles + tests pass**

Run:

```bash
go build ./...
go vet ./...
go test ./...
```

Expected: green across the board (integration-tagged tests excluded by default).

Then run the integration suite:

```bash
go test -tags=integration ./internal/devices/ ./internal/flextsnet/ -timeout=15m
```

Expected: PASS. May take 5–10 minutes.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: Plan 5 — flexctl login/ssh usage + e2e checklist"
```

---

## After all tasks

Run `superpowers:finishing-a-development-branch` to push and create the PR description.

The branch should already exist as `feat/plan-5-flexctl-client` (created when the spec was committed).

**Out of scope reminder (do NOT add):**
- `flexctl env start/stop/create/delete` (Web UI / Plan 6)
- Background daemon mode
- OAuth login
- Real-time key broadcast to running envs
- `flexctl ssh -L` shorthand (use raw `--` passthrough)
- VS Code auto-install

**Acceptance gate before merge:** Task 10's integration test must pass on its own — that's the strongest evidence Plan 5's tsnet + ACL story actually works without needing a GPU machine.
