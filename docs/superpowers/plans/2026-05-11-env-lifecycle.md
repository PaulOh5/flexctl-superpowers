# Environment Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 사용자가 `POST /v1/envs`로 환경 생성 요청하면 agent가 자기 GPU 서버에 사이드카(tailscaled+TUN) + dev(unprivileged sshd) 컨테이너 묶음을 띄워 Headscale tailnet에 가입시키고 status=running으로 만든다. Stop/Start/Delete + 얕은 resync 까지.

**Architecture:** 새 패키지 4개(`envs`, `imagetemplates`, `envdocker`, `envlifecycle`) + agent 측 Docker SDK wrapper + gRPC 메시지 7종 추가 + 사이드카/dev Dockerfile 2종. 라벨이 agent의 권위 소스 — CreateEnv 시 `flexctl.*` 라벨에 모든 재구성 정보를 박아둠. Plan 1-3 그대로 사용.

**Tech Stack:** Go 1.22+, docker SDK(`github.com/docker/docker/client`), 기존 chi/pgx/grpc/protobuf 그대로. 새 외부 의존성: Docker daemon, NVIDIA Container Toolkit(GPU 패스스루용, 부재해도 gpu_request=0이면 동작).

**Out of scope (Plan 4.5+):** orphan 컨테이너 정리, error→retry 흐름, 사이드카 단독 crash 자동 재기동, 다중 dev 템플릿, GHCR push 자동화.

---

## File Structure

```
.
├── images/
│   ├── sidecar/
│   │   └── Dockerfile                       # NEW Alpine + tailscaled + flexctl sidecar
│   └── dev-cuda-base/
│       ├── Dockerfile                       # NEW nvidia/cuda + sshd
│       └── entrypoint.sh                    # NEW authorized_keys 주입 후 sshd
├── proto/
│   └── agent.proto                          # MODIFY: 7개 env 메시지 추가
├── internal/
│   ├── agentpb/                             # MODIFY (regen 후 commit)
│   ├── agentstream/server.go                # MODIFY: env 명령 dispatch + ack
│   ├── envdocker/                           # NEW
│   │   ├── client.go                        # DockerClient 인터페이스 + RealDockerClient
│   │   ├── client_test.go                   # mock 단위
│   │   ├── integration_test.go              # //go:build integration
│   │   ├── labels.go                        # 라벨 키 상수 + 헬퍼
│   │   └── mock.go                          # MockDockerClient (test 외 일반 패키지)
│   ├── envlifecycle/                        # NEW
│   │   ├── allocator.go                     # GPUAllocator
│   │   ├── allocator_test.go
│   │   ├── dispatcher.go                    # CreateEnv/Stop/Start/Delete + watcher stub
│   │   └── dispatcher_test.go
│   ├── envs/                                # NEW
│   │   ├── service.go                       # DB CRUD + state machine
│   │   ├── service_test.go
│   │   ├── handlers.go                      # HTTP
│   │   └── handlers_test.go
│   ├── imagetemplates/                      # NEW
│   │   ├── service.go
│   │   └── service_test.go
│   ├── flexctlagent/agent.go                # MODIFY: ControlMessage 라우팅 + EnvStateSnapshot
│   └── flexctlcli/sidecar.go                # NEW `flexctl sidecar` 서브커맨드
├── migrations/
│   ├── 0006_image_templates.up.sql + down.sql
│   ├── 0007_envs.up.sql + down.sql
│   └── 0008_seed_templates.up.sql + down.sql
├── cmd/control-plane/main.go                # MODIFY: envs 라우트 + Headscale 키 발급기 와이어링
└── Makefile                                 # MODIFY: sidecar-image / dev-image 타겟
```

---

### Task 1: 마이그레이션 (image_templates + envs + seed)

**Files:**
- Create: `migrations/0006_image_templates.up.sql` / `down.sql`
- Create: `migrations/0007_envs.up.sql` / `down.sql`
- Create: `migrations/0008_seed_templates.up.sql` / `down.sql`

- [ ] **Step 1: 0006_image_templates**

`migrations/0006_image_templates.up.sql`:
```sql
CREATE TABLE image_templates (
  id            text PRIMARY KEY,
  display_name  text NOT NULL,
  description   text NOT NULL DEFAULT '',
  image_ref     text NOT NULL,
  default_cmd   text[] NOT NULL DEFAULT '{}',
  enabled       bool NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now()
);
```

`migrations/0006_image_templates.down.sql`:
```sql
DROP TABLE IF EXISTS image_templates;
```

- [ ] **Step 2: 0007_envs**

`migrations/0007_envs.up.sql`:
```sql
CREATE TABLE envs (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id              uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
  template_id          text NOT NULL REFERENCES image_templates(id),
  name                 text NOT NULL,
  hostname             text NOT NULL,
  status               text NOT NULL DEFAULT 'creating',
  status_message       text NOT NULL DEFAULT '',
  sidecar_container_id text NOT NULL DEFAULT '',
  dev_container_id     text NOT NULL DEFAULT '',
  gpu_request          int  NOT NULL DEFAULT 1,
  gpu_indices          int[] NOT NULL DEFAULT '{}',
  volume_name          text NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE(owner_user_id, name)
);
CREATE INDEX envs_node_status_idx ON envs (node_id, status);
CREATE INDEX envs_owner_idx       ON envs (owner_user_id);
```

`migrations/0007_envs.down.sql`:
```sql
DROP TABLE IF EXISTS envs;
```

- [ ] **Step 3: 0008_seed_templates**

`migrations/0008_seed_templates.up.sql`:
```sql
INSERT INTO image_templates (id, display_name, description, image_ref) VALUES
  ('cuda-base',
   'CUDA Base (Ubuntu 22.04 + CUDA 12.4)',
   'Minimal NVIDIA CUDA runtime on Ubuntu. Install PyTorch/TF via pip inside the container.',
   'flex/dev-cuda-base:dev')
ON CONFLICT (id) DO NOTHING;
```

`migrations/0008_seed_templates.down.sql`:
```sql
DELETE FROM image_templates WHERE id = 'cuda-base';
```

- [ ] **Step 4: 검증**

```bash
docker compose up -d postgres
make migrate-up
docker compose exec postgres psql -U flex -d flex -c '\d envs'
docker compose exec postgres psql -U flex -d flex -c '\d image_templates'
docker compose exec postgres psql -U flex -d flex -c "SELECT id, display_name FROM image_templates;"
# Expected: cuda-base row
make migrate-down  # 0008 down
make migrate-down  # 0007 down
make migrate-down  # 0006 down
make migrate-up    # 셋 다 복원
```

- [ ] **Step 5: Commit**

```bash
git add migrations/0006_image_templates.up.sql migrations/0006_image_templates.down.sql \
        migrations/0007_envs.up.sql migrations/0007_envs.down.sql \
        migrations/0008_seed_templates.up.sql migrations/0008_seed_templates.down.sql
git commit -m "feat(db): envs + image_templates + cuda-base seed migrations"
```

---

### Task 2: imagetemplates 패키지 (read-only)

**Files:**
- Create: `internal/imagetemplates/service.go`
- Create: `internal/imagetemplates/service_test.go`

- [ ] **Step 1: 실패 테스트**

`internal/imagetemplates/service_test.go`:
```go
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
```

- [ ] **Step 2: 실행으로 실패 확인**

Run: `go test ./internal/imagetemplates/ -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/imagetemplates/service.go`:
```go
package imagetemplates

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("image template not found")

type Template struct {
	ID          string
	DisplayName string
	Description string
	ImageRef    string
	DefaultCmd  []string
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) List(ctx context.Context) ([]Template, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, display_name, description, image_ref, default_cmd
		 FROM image_templates WHERE enabled = true ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.DisplayName, &t.Description, &t.ImageRef, &t.DefaultCmd); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) ByID(ctx context.Context, id string) (Template, error) {
	var t Template
	err := s.pool.QueryRow(ctx,
		`SELECT id, display_name, description, image_ref, default_cmd
		 FROM image_templates WHERE id = $1 AND enabled = true`,
		id,
	).Scan(&t.ID, &t.DisplayName, &t.Description, &t.ImageRef, &t.DefaultCmd)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("select: %w", err)
	}
	return t, nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/imagetemplates/ -race -count=1 -v`
Expected: 4 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/imagetemplates
git commit -m "feat(imagetemplates): read-only service with enabled filter"
```

---

### Task 3: envs 서비스 — DB CRUD + 상태 기계

**Files:**
- Create: `internal/envs/service.go`
- Create: `internal/envs/service_test.go`

- [ ] **Step 1: 실패 테스트**

`internal/envs/service_test.go`:
```go
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
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "a", GPURequest: 1,
	})
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "b", GPURequest: 1,
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
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "a", GPURequest: 1,
	})
	e2, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "b", GPURequest: 1,
	})
	_ = svc.MarkRunning(context.Background(), e1.ID, "s1", "d1", []int{0})
	_ = svc.MarkRunning(context.Background(), e2.ID, "s2", "d2", []int{1})
	// MarkStopped on e2
	_ = svc.MarkStopped(context.Background(), e2.ID)

	got, err := svc.ListRunningOnNode(context.Background(), nid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, e1.ID, got[0].ID)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/envs/ -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/envs/service.go`:
```go
package envs

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidName  = errors.New("invalid env name")
	ErrNameTaken    = errors.New("env name already taken")
	ErrNotFound     = errors.New("env not found")
	ErrInvalidState = errors.New("invalid state transition")
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

type Env struct {
	ID                 uuid.UUID
	OwnerUserID        uuid.UUID
	NodeID             uuid.UUID
	TemplateID         string
	Name               string
	Hostname           string
	Status             string
	StatusMessage      string
	SidecarContainerID string
	DevContainerID     string
	GPURequest         int32
	GPUIndices         []int32
	VolumeName         string
}

type CreateRequest struct {
	OwnerUserID uuid.UUID
	NodeID      uuid.UUID
	TemplateID  string
	Name        string
	GPURequest  int32
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, req CreateRequest) (Env, error) {
	if !nameRe.MatchString(req.Name) {
		return Env{}, ErrInvalidName
	}
	// hostname = <user-slug>-<env-name>, computed via JOIN
	var hostname string
	err := s.pool.QueryRow(ctx,
		`SELECT slug || '-' || $2 FROM users WHERE id = $1`,
		req.OwnerUserID, req.Name,
	).Scan(&hostname)
	if err != nil {
		return Env{}, fmt.Errorf("compute hostname: %w", err)
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO envs (owner_user_id, node_id, template_id, name, hostname,
		                   gpu_request, volume_name)
		 VALUES ($1, $2, $3, $4, $5, $6, '')
		 RETURNING id`,
		req.OwnerUserID, req.NodeID, req.TemplateID, req.Name, hostname, req.GPURequest,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Env{}, ErrNameTaken
		}
		return Env{}, fmt.Errorf("insert: %w", err)
	}
	volumeName := "flex-env-" + id.String()
	_, err = s.pool.Exec(ctx, `UPDATE envs SET volume_name = $2 WHERE id = $1`, id, volumeName)
	if err != nil {
		return Env{}, fmt.Errorf("set volume: %w", err)
	}
	return Env{
		ID:          id,
		OwnerUserID: req.OwnerUserID,
		NodeID:      req.NodeID,
		TemplateID:  req.TemplateID,
		Name:        req.Name,
		Hostname:    hostname,
		Status:      "creating",
		GPURequest:  req.GPURequest,
		VolumeName:  volumeName,
	}, nil
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (Env, error) {
	var e Env
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_user_id, node_id, template_id, name, hostname,
		       status, status_message, sidecar_container_id, dev_container_id,
		       gpu_request, gpu_indices, volume_name
		FROM envs WHERE id = $1`, id,
	).Scan(&e.ID, &e.OwnerUserID, &e.NodeID, &e.TemplateID, &e.Name, &e.Hostname,
		&e.Status, &e.StatusMessage, &e.SidecarContainerID, &e.DevContainerID,
		&e.GPURequest, &e.GPUIndices, &e.VolumeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Env{}, ErrNotFound
	}
	if err != nil {
		return Env{}, fmt.Errorf("select: %w", err)
	}
	return e, nil
}

func (s *Service) ListByOwner(ctx context.Context, userID uuid.UUID) ([]Env, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_user_id, node_id, template_id, name, hostname,
		       status, status_message, sidecar_container_id, dev_container_id,
		       gpu_request, gpu_indices, volume_name
		FROM envs WHERE owner_user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		var e Env
		if err := rows.Scan(&e.ID, &e.OwnerUserID, &e.NodeID, &e.TemplateID, &e.Name, &e.Hostname,
			&e.Status, &e.StatusMessage, &e.SidecarContainerID, &e.DevContainerID,
			&e.GPURequest, &e.GPUIndices, &e.VolumeName); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) ListRunningOnNode(ctx context.Context, nodeID uuid.UUID) ([]Env, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_user_id, node_id, template_id, name, hostname,
		       status, status_message, sidecar_container_id, dev_container_id,
		       gpu_request, gpu_indices, volume_name
		FROM envs WHERE node_id = $1 AND status = 'running'`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		var e Env
		if err := rows.Scan(&e.ID, &e.OwnerUserID, &e.NodeID, &e.TemplateID, &e.Name, &e.Hostname,
			&e.Status, &e.StatusMessage, &e.SidecarContainerID, &e.DevContainerID,
			&e.GPURequest, &e.GPUIndices, &e.VolumeName); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) MarkStatus(ctx context.Context, id uuid.UUID, status string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE envs SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkRunning(ctx context.Context, id uuid.UUID, sidecarID, devID string, gpuIdx []int) error {
	idx32 := make([]int32, 0, len(gpuIdx))
	for _, i := range gpuIdx {
		idx32 = append(idx32, int32(i))
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE envs
		SET status = 'running', status_message = '',
		    sidecar_container_id = $2, dev_container_id = $3, gpu_indices = $4,
		    updated_at = now()
		WHERE id = $1`,
		id, sidecarID, devID, idx32)
	if err != nil {
		return fmt.Errorf("mark running: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkStopped(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE envs
		SET status = 'stopped', gpu_indices = '{}', updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark stopped: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkError(ctx context.Context, id uuid.UUID, stage, detail string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE envs
		SET status = 'error', status_message = $2, gpu_indices = '{}', updated_at = now()
		WHERE id = $1`, id, fmt.Sprintf("[%s] %s", stage, detail))
	if err != nil {
		return fmt.Errorf("mark error: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM envs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/envs/ -race -count=1 -v`
Expected: 9 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/envs
git commit -m "feat(envs): CRUD service with state machine"
```

---

### Task 4: envs HTTP 핸들러

**Files:**
- Create: `internal/envs/handlers.go`
- Create: `internal/envs/handlers_test.go`

NOTE: 이번 task는 HTTP 핸들러 + 소유권 검증만. Headscale 키 발급 + agent 명령 dispatch는 Task 5(main.go 와이어링)에서 외부에서 주입되는 인터페이스로 처리. handlers.go가 직접 Headscale을 부르지 않게 분리.

- [ ] **Step 1: 실패 테스트 작성**

`internal/envs/handlers_test.go`:
```go
package envs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/envs"
)

// fakeDispatcher records calls without actually sending to an agent.
type fakeDispatcher struct {
	mu       sync.Mutex
	creates  []uuid.UUID
	stops    []uuid.UUID
	starts   []uuid.UUID
	deletes  []uuid.UUID
	failNext error
}

func (f *fakeDispatcher) Create(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock()
	if f.failNext != nil { e := f.failNext; f.failNext = nil; return e }
	f.creates = append(f.creates, envID); return nil
}
func (f *fakeDispatcher) Stop(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.stops = append(f.stops, envID); return nil
}
func (f *fakeDispatcher) Start(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.starts = append(f.starts, envID); return nil
}
func (f *fakeDispatcher) Delete(_ context.Context, envID uuid.UUID) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.deletes = append(f.deletes, envID); return nil
}

func mountAuthed(t *testing.T, signer *auth.SessionSigner, h *envs.Handlers) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.Mount(r)
	})
	return httptest.NewServer(r)
}

func TestPostEnvs_HappyPath(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()

	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id":     nid.String(),
		"template_id": "cuda-base",
		"name":        "vllm",
		"gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, disp.creates, 1)
}

func TestPostEnvs_DispatcherFailsRollsBack(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	disp := &fakeDispatcher{failNext: errors.New("agent offline")}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()

	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id": nid.String(), "template_id": "cuda-base", "name": "vllm", "gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	// row rolled back
	got, _ := svc.ListByOwner(context.Background(), uid)
	require.Len(t, got, 0)
}

func TestPostEnvs_DuplicateName409(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	_, err := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "dup", GPURequest: 1,
	})
	require.NoError(t, err)

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id": nid.String(), "template_id": "cuda-base", "name": "dup", "gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestStopHandler_HappyPath(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs/"+env.ID.String()+"/stop", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Equal(t, []uuid.UUID{env.ID}, disp.stops)
}

func TestStopHandler_NotOwner404(t *testing.T) {
	pool := newPool(t)
	uidA, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	env, _ := svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uidA, NodeID: nid, TemplateID: "cuda-base", Name: "e1", GPURequest: 1,
	})

	// User B
	otherID := uuid.New()
	_, _ = pool.Exec(context.Background(),
		`INSERT INTO users (id, email, slug, password_hash) VALUES ($1, 'b@x.com', 'bob', 'x')`, otherID)

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tokB, _ := signer.Encode(auth.Session{UserID: otherID, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/envs/"+env.ID.String()+"/stop", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tokB})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Empty(t, disp.stops)
}

func TestListEnvs(t *testing.T) {
	pool := newPool(t)
	uid, nid := mkUserAndNode(t, pool)
	svc := envs.NewService(pool)
	_, _ = svc.Create(context.Background(), envs.CreateRequest{
		OwnerUserID: uid, NodeID: nid, TemplateID: "cuda-base", Name: "a", GPURequest: 1,
	})

	disp := &fakeDispatcher{}
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := envs.NewHandlers(svc, disp)
	srv := mountAuthed(t, signer, h)
	defer srv.Close()
	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/envs", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got, 1)
	require.Equal(t, "a", got[0]["name"])
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/envs/ -run "TestPost|TestStop|TestList" -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/envs/handlers.go`:
```go
package envs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

// Dispatcher dispatches env lifecycle commands to the relevant agent. The
// production impl uses the agent gRPC stream; tests pass a fake.
type Dispatcher interface {
	Create(ctx context.Context, envID uuid.UUID) error
	Stop(ctx context.Context, envID uuid.UUID) error
	Start(ctx context.Context, envID uuid.UUID) error
	Delete(ctx context.Context, envID uuid.UUID) error
}

type Handlers struct {
	svc  *Service
	disp Dispatcher
}

func NewHandlers(svc *Service, disp Dispatcher) *Handlers {
	return &Handlers{svc: svc, disp: disp}
}

func (h *Handlers) Mount(r chi.Router) {
	r.Get("/v1/envs", h.list)
	r.Post("/v1/envs", h.create)
	r.Get("/v1/envs/{id}", h.get)
	r.Post("/v1/envs/{id}/stop", h.stop)
	r.Post("/v1/envs/{id}/start", h.start)
	r.Delete("/v1/envs/{id}", h.delete)
}

type createReq struct {
	NodeID     string `json:"node_id"`
	TemplateID string `json:"template_id"`
	Name       string `json:"name"`
	GPURequest int32  `json:"gpu_request"`
}

type envResp struct {
	ID                 string  `json:"id"`
	NodeID             string  `json:"node_id"`
	TemplateID         string  `json:"template_id"`
	Name               string  `json:"name"`
	Hostname           string  `json:"hostname"`
	Status             string  `json:"status"`
	StatusMessage      string  `json:"status_message,omitempty"`
	SidecarContainerID string  `json:"sidecar_container_id,omitempty"`
	DevContainerID     string  `json:"dev_container_id,omitempty"`
	GPURequest         int32   `json:"gpu_request"`
	GPUIndices         []int32 `json:"gpu_indices"`
}

func toResp(e Env) envResp {
	return envResp{
		ID: e.ID.String(), NodeID: e.NodeID.String(), TemplateID: e.TemplateID,
		Name: e.Name, Hostname: e.Hostname, Status: e.Status,
		StatusMessage: e.StatusMessage, SidecarContainerID: e.SidecarContainerID,
		DevContainerID: e.DevContainerID, GPURequest: e.GPURequest,
		GPUIndices: e.GPUIndices,
	}
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json"); return
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid node_id"); return
	}
	if req.GPURequest < 0 {
		httperr.Write(w, http.StatusBadRequest, "gpu_request must be >= 0"); return
	}
	env, err := h.svc.Create(r.Context(), CreateRequest{
		OwnerUserID: uid, NodeID: nodeID, TemplateID: req.TemplateID,
		Name: req.Name, GPURequest: req.GPURequest,
	})
	switch {
	case errors.Is(err, ErrInvalidName):
		httperr.Write(w, http.StatusBadRequest, "invalid env name"); return
	case errors.Is(err, ErrNameTaken):
		httperr.Write(w, http.StatusConflict, "env name taken"); return
	case err != nil:
		slog.Error("envs.Create", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}

	// Dispatch to agent. On failure, roll back DB row.
	if err := h.disp.Create(r.Context(), env.ID); err != nil {
		slog.Error("dispatch create", "err", err, "env_id", env.ID)
		if delErr := h.svc.Delete(r.Context(), env.ID); delErr != nil {
			slog.Error("rollback delete", "err", delErr, "env_id", env.ID)
		}
		httperr.Write(w, http.StatusInternalServerError, "dispatch failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(toResp(env))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	got, err := h.svc.ListByOwner(r.Context(), uid)
	if err != nil {
		slog.Error("envs.ListByOwner", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	out := make([]envResp, 0, len(got))
	for _, e := range got {
		out = append(out, toResp(e))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id"); return
	}
	env, err := h.svc.ByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && env.OwnerUserID != uid) {
		httperr.Write(w, http.StatusNotFound, "not found"); return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toResp(env))
}

func (h *Handlers) stop(w http.ResponseWriter, r *http.Request)   { h.action(w, r, h.disp.Stop) }
func (h *Handlers) start(w http.ResponseWriter, r *http.Request)  { h.action(w, r, h.disp.Start) }
func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) { h.action(w, r, h.disp.Delete) }

func (h *Handlers) action(w http.ResponseWriter, r *http.Request, fn func(context.Context, uuid.UUID) error) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id"); return
	}
	env, err := h.svc.ByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && env.OwnerUserID != uid) {
		httperr.Write(w, http.StatusNotFound, "not found"); return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error"); return
	}
	if err := fn(r.Context(), id); err != nil {
		slog.Error("dispatch", "err", err, "env_id", id)
		httperr.Write(w, http.StatusInternalServerError, "dispatch failed"); return
	}
	w.WriteHeader(http.StatusAccepted)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/envs/ -race -count=1 -v`
Expected: 모든 service + handler 테스트 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/envs/handlers.go internal/envs/handlers_test.go
git commit -m "feat(envs): http handlers with dispatcher interface"
```

---

### Task 5: Headscale 키 발급기 + main.go에 envs 와이어링

NOTE: Dispatcher 실제 구현은 agentstream(Task 12) 에서. 이번 task는 (a) Headscale ephemeral key 발급 helper를 컨트롤 플레인에 만들고, (b) envs 라우트를 main.go에 등록한다. Dispatcher는 임시로 stub(`disp := &noopDispatcher{}`)으로 와이어링하고, Task 12에서 진짜 구현으로 교체.

**Files:**
- Modify: `internal/headscale/client.go` (또는 `internal/headscale/preauthkey.go` 새 파일) — `CreatePreAuthKey` 메서드 추가
- Modify: `internal/headscale/client_test.go` (또는 `preauthkey_test.go` 새 파일)
- Modify: `cmd/control-plane/main.go`

- [ ] **Step 1: Headscale CreatePreAuthKey 실패 테스트 추가**

`internal/headscale/client_test.go` 끝에 추가:
```go
func TestCreatePreAuthKey_Ephemeral(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	// user 먼저 만들고
	_, err := c.CreateUser(ctx, "paul")
	require.NoError(t, err)

	key, err := c.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User:       "paul",
		Reusable:   false,
		Ephemeral:  true,
		Expiration: 24 * time.Hour,
		ACLTags:    []string{"tag:env-paul"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, key.Key)
	require.True(t, key.Ephemeral)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/headscale/ -run TestCreatePreAuthKey -v`
Expected: 컴파일 에러.

- [ ] **Step 3: CreatePreAuthKey 구현**

`internal/headscale/client.go` 끝에 추가:
```go
type PreAuthKeyRequest struct {
	User       string
	Reusable   bool
	Ephemeral  bool
	Expiration time.Duration
	ACLTags    []string
}

type PreAuthKey struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Ephemeral  bool      `json:"ephemeral"`
	Reusable   bool      `json:"reusable"`
	Expiration time.Time `json:"expiration"`
	CreatedAt  time.Time `json:"createdAt"`
}

type preAuthKeyHTTPReq struct {
	User       string   `json:"user"`
	Reusable   bool     `json:"reusable"`
	Ephemeral  bool     `json:"ephemeral"`
	Expiration string   `json:"expiration"` // RFC 3339
	ACLTags    []string `json:"aclTags"`
}

type preAuthKeyHTTPResp struct {
	PreAuthKey PreAuthKey `json:"preAuthKey"`
}

func (c *Client) CreatePreAuthKey(ctx context.Context, req PreAuthKeyRequest) (PreAuthKey, error) {
	body := preAuthKeyHTTPReq{
		User: req.User, Reusable: req.Reusable, Ephemeral: req.Ephemeral,
		Expiration: time.Now().Add(req.Expiration).UTC().Format(time.RFC3339),
		ACLTags:    req.ACLTags,
	}
	var out preAuthKeyHTTPResp
	if err := c.do(ctx, http.MethodPost, "/api/v1/preauthkey", body, &out); err != nil {
		return PreAuthKey{}, err
	}
	return out.PreAuthKey, nil
}
```

NOTE: Headscale 0.23 API는 `expiration`을 ISO 8601/RFC 3339 문자열로 받음. 위 `Format(time.RFC3339)`로 처리. 만약 그 API가 duration 문자열(`"24h"`)을 받는 버전이면 테스트가 실패하며 명시적 에러를 보임 — 그 경우 plan 실행자가 `"24h"` 형태로 수정.

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/headscale/ -race -count=1 -v`
Expected: 모두 PASS (기존 + 신규).

- [ ] **Step 5: cmd/control-plane/main.go 수정**

import 블록에 추가:
```go
	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/imagetemplates"
```

기존 `nodesH := nodes.NewHandlers(nodesSvc)` 다음 줄에 추가:
```go
	envsSvc := envs.NewService(pool)
	tplSvc := imagetemplates.NewService(pool)

	// Stub dispatcher — Task 12에서 agentstream 기반 구현으로 교체
	envDispatcher := &noopEnvDispatcher{}
	envsH := envs.NewHandlers(envsSvc, envDispatcher)

	// /v1/image-templates 핸들러 (간단히 inline — 별도 패키지 안 만듦)
	imageTemplatesHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tpls, err := tplSvc.List(r.Context())
		if err != nil {
			slog.Error("image templates list", "err", err)
			httperr.Write(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tpls)
	})
```

기존 인증 그룹 안에 다음 두 줄 추가:
```go
		envsH.Mount(r)
		r.Get("/v1/image-templates", imageTemplatesHandler)
```

같은 파일 끝에 stub 정의 추가:
```go
// noopEnvDispatcher는 Task 12에서 agentstream 기반 구현으로 교체된다.
// 그때까지 컨트롤 플레인은 envs 행만 만들고 agent에는 명령을 보내지 않는다 — env가 영원히 creating 상태로 남음.
type noopEnvDispatcher struct{}

func (noopEnvDispatcher) Create(_ context.Context, _ uuid.UUID) error { return nil }
func (noopEnvDispatcher) Stop(_ context.Context, _ uuid.UUID) error   { return nil }
func (noopEnvDispatcher) Start(_ context.Context, _ uuid.UUID) error  { return nil }
func (noopEnvDispatcher) Delete(_ context.Context, _ uuid.UUID) error { return nil }
```

`uuid`, `httperr`, `json` import 필요하면 추가.

- [ ] **Step 6: 빌드 + 수동 검증**

```bash
make build
docker compose up -d postgres headscale
make migrate-up
make headscale-init 2>&1 | tail -3
KEY=<paste>

FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
FLEX_HEADSCALE_API_KEY="$KEY" \
./bin/control-plane &
sleep 1

curl -s -X POST http://localhost:8080/v1/auth/signup \
  -H 'content-type: application/json' \
  -d '{"email":"e@x.com","slug":"paul","password":"correct-horse-battery"}' \
  -c /tmp/c.txt > /dev/null

# templates 보임?
curl -i -b /tmp/c.txt http://localhost:8080/v1/image-templates
# Expected: 200 + [{"ID":"cuda-base", ...}]

# env 생성 (noop dispatcher라 row만 만들어짐, agent 없음)
NODE_TOKEN_PT=$(curl -s -X POST http://localhost:8080/v1/nodes/pair-token -b /tmp/c.txt | jq -r .token)
PAIR_RESP=$(curl -s -X POST http://localhost:8080/v1/nodes/pair \
  -H 'content-type: application/json' \
  -d "{\"token\":\"$NODE_TOKEN_PT\",\"name\":\"manual\",\"gpu_info\":[]}")
NODE_ID=$(echo "$PAIR_RESP" | jq -r .node_id)
curl -i -b /tmp/c.txt -X POST http://localhost:8080/v1/envs \
  -H 'content-type: application/json' \
  -d "{\"node_id\":\"$NODE_ID\",\"template_id\":\"cuda-base\",\"name\":\"first\",\"gpu_request\":1}"
# Expected: 202 + env JSON, status=creating

pkill -f bin/control-plane
```

- [ ] **Step 7: Commit**

```bash
git add internal/headscale/client.go internal/headscale/client_test.go cmd/control-plane/main.go
git commit -m "feat(control-plane): wire envs routes + Headscale preauthkey API"
```

---

### Task 6: Proto 확장 — 7개 env 메시지

**Files:**
- Modify: `proto/agent.proto`
- Modify: `internal/agentpb/agent.pb.go` (regen + commit)
- Modify: `internal/agentpb/agent_grpc.pb.go` (regen + commit)

- [ ] **Step 1: proto 파일 수정**

`proto/agent.proto`의 기존 `AgentMessage`와 `ControlMessage` `oneof payload`에 추가, 그리고 새 message 정의:

```proto
// 기존 message AgentMessage { oneof payload { ... 기존 ... } } 안에 필드 추가:
//   EnvReady env_ready                = 3;
//   EnvStopped env_stopped            = 4;
//   EnvDeleted env_deleted            = 5;
//   EnvError env_error                = 6;
//   EnvStateSnapshot env_snapshot     = 7;

// 기존 message ControlMessage { oneof payload { ... } } 안에 필드 추가:
//   CreateEnv create_env  = 3;
//   StopEnv   stop_env    = 4;
//   StartEnv  start_env   = 5;
//   DeleteEnv delete_env  = 6;

// 그리고 파일 끝에 새 메시지 정의 추가:
message CreateEnv {
  string env_id            = 1;
  string image_ref         = 2;
  string sidecar_image_ref = 3;
  string hostname          = 4;
  string headscale_url     = 5;
  string preauth_key       = 6;
  repeated string tags     = 7;
  string authorized_keys   = 8;
  int32  gpu_request       = 9;
  repeated string default_cmd = 10;
}

message StopEnv  { string env_id = 1; }

message StartEnv {
  string env_id          = 1;
  string preauth_key     = 2;
  string authorized_keys = 3;
}

message DeleteEnv { string env_id = 1; }

message EnvReady {
  string env_id              = 1;
  string sidecar_container_id = 2;
  string dev_container_id     = 3;
  repeated int32 gpu_indices  = 4;
}

message EnvStopped { string env_id = 1; }

message EnvDeleted { string env_id = 1; }

message EnvError {
  string env_id = 1;
  string stage  = 2;
  string detail = 3;
}

message EnvStateSnapshot {
  repeated string running_env_ids = 1;
}
```

기존 oneof들의 정확한 텍스트는 파일을 열어 확인. 신규 필드 번호는 기존 max + 1부터 계속.

- [ ] **Step 2: regen**

```bash
make proto-gen
```

- [ ] **Step 3: 빌드 확인**

```bash
go build ./...
```
Expected: 클린. 기존 agentstream 코드는 oneof 새 필드 미사용이라 영향 없음.

- [ ] **Step 4: 신규 타입 존재 확인**

```bash
grep -E '^type (CreateEnv|StopEnv|StartEnv|DeleteEnv|EnvReady|EnvStopped|EnvDeleted|EnvError|EnvStateSnapshot) struct' internal/agentpb/agent.pb.go | wc -l
```
Expected: 9.

- [ ] **Step 5: 테스트**

Run: `go test ./... -race -count=1`
Expected: 모두 PASS.

- [ ] **Step 6: Commit**

```bash
git add proto/agent.proto internal/agentpb/agent.pb.go internal/agentpb/agent_grpc.pb.go
git commit -m "feat(proto): env lifecycle messages (CreateEnv/Stop/Start/Delete + Ready/Stopped/Deleted/Error/StateSnapshot)"
```

---

### Task 7: envdocker — DockerClient 인터페이스 + Real + Mock

**Files:**
- Create: `internal/envdocker/client.go`
- Create: `internal/envdocker/labels.go`
- Create: `internal/envdocker/mock.go`
- Create: `internal/envdocker/client_test.go` (mock 단위)

- [ ] **Step 1: Docker SDK 의존성 추가**

```bash
go get github.com/docker/docker@v27.3.1+incompatible
go mod tidy
```

- [ ] **Step 2: 라벨 상수 작성**

`internal/envdocker/labels.go`:
```go
package envdocker

// 라벨 키 — agent의 권위 소스. CreateEnv 시 컨테이너에 박아두면
// Start/Delete/RestoreFromDocker 가 docker inspect로 자급자족 가능.
const (
	LabelEnvID         = "flexctl.env_id"
	LabelRole          = "flexctl.role"          // "sidecar" | "dev"
	LabelHostname      = "flexctl.hostname"
	LabelHeadscaleURL  = "flexctl.headscale_url"
	LabelTags          = "flexctl.tags"          // csv
	LabelImageRef      = "flexctl.image_ref"
	LabelGPUIndices    = "flexctl.gpu_indices"   // csv ints, "" if none
	LabelVolumeName    = "flexctl.volume_name"
)
```

- [ ] **Step 3: 인터페이스 + Mock 작성**

`internal/envdocker/client.go`:
```go
package envdocker

import (
	"context"
	"io"
	"time"
)

type ContainerSpec struct {
	Name         string
	Image        string
	Cmd          []string
	Env          map[string]string
	Labels       map[string]string
	NetworkMode  string
	CapAdd       []string
	Devices      []string
	GPUIndices   []int
	VolumeMounts []VolumeMount
}

type VolumeMount struct {
	Source string
	Target string
}

type ContainerInfo struct {
	ID     string
	Name   string
	State  string
	Labels map[string]string
	Env    []string
}

type DockerClient interface {
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (ContainerInfo, error)
	ListContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerInfo, error)
	Exec(ctx context.Context, id string, cmd []string) (stdout, stderr io.Reader, exitCode int, err error)
	RemoveVolume(ctx context.Context, name string) error
	PullImage(ctx context.Context, ref string) error
}
```

`internal/envdocker/mock.go`:
```go
package envdocker

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// MockDockerClient is a deterministic in-memory implementation for unit tests.
// All methods are safe to call concurrently. Test helpers record calls and let
// the test inject failures.
type MockDockerClient struct {
	mu         sync.Mutex
	Containers map[string]ContainerInfo // keyed by ID (assigned at create)
	Volumes    map[string]bool          // name → exists
	Calls      []string                 // sequence of method names
	NextErrors map[string]error         // method name → error to return next, then cleared
	ExecOutput map[string]string        // container ID → stdout for next Exec
	ExecExit   map[string]int           // container ID → exit code
	idCounter  int
}

func NewMockDockerClient() *MockDockerClient {
	return &MockDockerClient{
		Containers: map[string]ContainerInfo{},
		Volumes:    map[string]bool{},
		NextErrors: map[string]error{},
		ExecOutput: map[string]string{},
		ExecExit:   map[string]int{},
	}
}

func (m *MockDockerClient) take(method string) error {
	m.Calls = append(m.Calls, method)
	if e, ok := m.NextErrors[method]; ok {
		delete(m.NextErrors, method)
		return e
	}
	return nil
}

func (m *MockDockerClient) CreateContainer(_ context.Context, spec ContainerSpec) (string, error) {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("CreateContainer:" + spec.Name); err != nil { return "", err }
	m.idCounter++
	id := spec.Name + "-id"
	envSlice := []string{}
	for k, v := range spec.Env { envSlice = append(envSlice, k+"="+v) }
	m.Containers[id] = ContainerInfo{
		ID: id, Name: spec.Name, State: "created", Labels: spec.Labels, Env: envSlice,
	}
	return id, nil
}

func (m *MockDockerClient) StartContainer(_ context.Context, id string) error {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("StartContainer:" + id); err != nil { return err }
	c, ok := m.Containers[id]
	if !ok { return errors.New("not found") }
	c.State = "running"
	m.Containers[id] = c
	return nil
}

func (m *MockDockerClient) StopContainer(_ context.Context, id string, _ time.Duration) error {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("StopContainer:" + id); err != nil { return err }
	c, ok := m.Containers[id]
	if !ok { return errors.New("not found") }
	c.State = "exited"
	m.Containers[id] = c
	return nil
}

func (m *MockDockerClient) RemoveContainer(_ context.Context, id string, _ bool) error {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("RemoveContainer:" + id); err != nil { return err }
	delete(m.Containers, id)
	return nil
}

func (m *MockDockerClient) InspectContainer(_ context.Context, id string) (ContainerInfo, error) {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("InspectContainer:" + id); err != nil { return ContainerInfo{}, err }
	c, ok := m.Containers[id]
	if !ok { return ContainerInfo{}, errors.New("not found") }
	return c, nil
}

func (m *MockDockerClient) ListContainers(_ context.Context, labelFilter map[string]string) ([]ContainerInfo, error) {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("ListContainers"); err != nil { return nil, err }
	var out []ContainerInfo
	for _, c := range m.Containers {
		match := true
		for k, v := range labelFilter {
			if c.Labels[k] != v { match = false; break }
		}
		if match { out = append(out, c) }
	}
	return out, nil
}

func (m *MockDockerClient) Exec(_ context.Context, id string, _ []string) (io.Reader, io.Reader, int, error) {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("Exec:" + id); err != nil { return nil, nil, 0, err }
	stdout := m.ExecOutput[id]
	exit := m.ExecExit[id]
	return strings.NewReader(stdout), strings.NewReader(""), exit, nil
}

func (m *MockDockerClient) RemoveVolume(_ context.Context, name string) error {
	m.mu.Lock(); defer m.mu.Unlock()
	if err := m.take("RemoveVolume:" + name); err != nil { return err }
	delete(m.Volumes, name)
	return nil
}

func (m *MockDockerClient) PullImage(_ context.Context, ref string) error {
	m.mu.Lock(); defer m.mu.Unlock()
	return m.take("PullImage:" + ref)
}
```

- [ ] **Step 4: Real 구현 추가 (client.go에 이어서)**

`internal/envdocker/client.go` 끝에 추가:
```go
import (
	"bytes"
	"errors"
	"fmt"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

type RealDockerClient struct {
	cli *client.Client
}

func NewRealDockerClient() (*RealDockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &RealDockerClient{cli: cli}, nil
}

func (r *RealDockerClient) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	cfg := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Cmd,
		Labels: spec.Labels,
		Env:    envMapToSlice(spec.Env),
	}
	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.NetworkMode),
		CapAdd:      spec.CapAdd,
	}
	for _, d := range spec.Devices {
		hostCfg.Devices = append(hostCfg.Devices, container.DeviceMapping{
			PathOnHost: d, PathInContainer: d, CgroupPermissions: "rwm",
		})
	}
	for _, m := range spec.VolumeMounts {
		hostCfg.Binds = append(hostCfg.Binds, m.Source+":"+m.Target)
	}
	if len(spec.GPUIndices) > 0 {
		ids := make([]string, 0, len(spec.GPUIndices))
		for _, i := range spec.GPUIndices { ids = append(ids, strconv.Itoa(i)) }
		hostCfg.Resources.DeviceRequests = []container.DeviceRequest{{
			Driver: "nvidia", Capabilities: [][]string{{"gpu"}}, DeviceIDs: ids,
		}}
	}
	resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil { return "", fmt.Errorf("create: %w", err) }
	return resp.ID, nil
}

func (r *RealDockerClient) StartContainer(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *RealDockerClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &secs})
}

func (r *RealDockerClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

func (r *RealDockerClient) InspectContainer(ctx context.Context, id string) (ContainerInfo, error) {
	j, err := r.cli.ContainerInspect(ctx, id)
	if err != nil { return ContainerInfo{}, fmt.Errorf("inspect: %w", err) }
	return ContainerInfo{
		ID: j.ID, Name: j.Name, State: j.State.Status,
		Labels: j.Config.Labels, Env: j.Config.Env,
	}, nil
}

func (r *RealDockerClient) ListContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerInfo, error) {
	args := filters.NewArgs()
	for k, v := range labelFilter { args.Add("label", k+"="+v) }
	list, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil { return nil, fmt.Errorf("list: %w", err) }
	out := make([]ContainerInfo, 0, len(list))
	for _, c := range list {
		out = append(out, ContainerInfo{
			ID: c.ID, Name: c.Names[0], State: c.State, Labels: c.Labels,
		})
	}
	return out, nil
}

func (r *RealDockerClient) Exec(ctx context.Context, id string, cmd []string) (io.Reader, io.Reader, int, error) {
	exec, err := r.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil { return nil, nil, 0, fmt.Errorf("exec create: %w", err) }
	att, err := r.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil { return nil, nil, 0, fmt.Errorf("exec attach: %w", err) }
	defer att.Close()
	var stdout, stderr bytes.Buffer
	if _, err := io.Copy(&stdout, att.Reader); err != nil {
		return nil, nil, 0, fmt.Errorf("read stdout: %w", err)
	}
	insp, err := r.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil { return nil, nil, 0, fmt.Errorf("exec inspect: %w", err) }
	return &stdout, &stderr, insp.ExitCode, nil
}

func (r *RealDockerClient) RemoveVolume(ctx context.Context, name string) error {
	return r.cli.VolumeRemove(ctx, name, false)
}

func (r *RealDockerClient) PullImage(ctx context.Context, ref string) error {
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil { return fmt.Errorf("pull: %w", err) }
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func envMapToSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m { out = append(out, k+"="+v) }
	return out
}

var _ = errors.New // import 보존
```

- [ ] **Step 5: Mock 기반 단위 테스트**

`internal/envdocker/client_test.go`:
```go
package envdocker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/envdocker"
)

func TestMockDockerClient_HappyPath(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	ctx := context.Background()

	id, err := m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-test", Image: "busybox", Labels: map[string]string{
			envdocker.LabelEnvID: "abc", envdocker.LabelRole: "dev",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "flex-env-test-id", id)
	require.NoError(t, m.StartContainer(ctx, id))

	got, err := m.InspectContainer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "running", got.State)
	require.Equal(t, "abc", got.Labels[envdocker.LabelEnvID])
}

func TestMockDockerClient_FilterByLabel(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	ctx := context.Background()
	_, _ = m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "a", Labels: map[string]string{envdocker.LabelRole: "sidecar"},
	})
	_, _ = m.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "b", Labels: map[string]string{envdocker.LabelRole: "dev"},
	})

	got, err := m.ListContainers(ctx, map[string]string{envdocker.LabelRole: "dev"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "b", got[0].Name)
}

func TestMockDockerClient_InjectError(t *testing.T) {
	m := envdocker.NewMockDockerClient()
	m.NextErrors["CreateContainer:flex-env-x"] = errors.New("pull failed")

	_, err := m.CreateContainer(context.Background(), envdocker.ContainerSpec{Name: "flex-env-x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pull failed")

	// Second call without injected error succeeds
	_, err = m.CreateContainer(context.Background(), envdocker.ContainerSpec{Name: "flex-env-y"})
	require.NoError(t, err)
}
```

- [ ] **Step 6: 통과 확인**

Run: `go test ./internal/envdocker/ -race -count=1 -v`
Expected: 3 PASS.

Run: `go build ./...` — 클린.

- [ ] **Step 7: Commit**

```bash
git add internal/envdocker go.mod go.sum
git commit -m "feat(envdocker): DockerClient interface + Real + Mock impl"
```

---

### Task 8: envdocker — //go:build integration 테스트

**Files:**
- Create: `internal/envdocker/integration_test.go`

이 task는 진짜 Docker daemon에서 alpine 컨테이너 lifecycle을 검증. CI/CD에서는 skip 또는 별도 job, 개발 머신에서는 `go test -tags integration` 으로 실행.

- [ ] **Step 1: 통합 테스트 작성**

`internal/envdocker/integration_test.go`:
```go
//go:build integration

package envdocker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/envdocker"
)

func TestRealDocker_AlpineLifecycle(t *testing.T) {
	c, err := envdocker.NewRealDockerClient()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, c.PullImage(ctx, "alpine:3.20"))

	id, err := c.CreateContainer(ctx, envdocker.ContainerSpec{
		Name:  "flex-envdocker-integration-" + time.Now().Format("150405"),
		Image: "alpine:3.20",
		Cmd:   []string{"sleep", "10"},
		Labels: map[string]string{
			envdocker.LabelEnvID: "integration-test",
			envdocker.LabelRole:  "dev",
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id, true) })

	require.NoError(t, c.StartContainer(ctx, id))

	info, err := c.InspectContainer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "running", info.State)
	require.Equal(t, "integration-test", info.Labels[envdocker.LabelEnvID])

	stdout, _, exit, err := c.Exec(ctx, id, []string{"echo", "hello"})
	require.NoError(t, err)
	require.Equal(t, 0, exit)
	buf := make([]byte, 100)
	n, _ := stdout.Read(buf)
	require.Contains(t, string(buf[:n]), "hello")

	list, err := c.ListContainers(ctx, map[string]string{envdocker.LabelEnvID: "integration-test"})
	require.NoError(t, err)
	require.NotEmpty(t, list)

	require.NoError(t, c.StopContainer(ctx, id, 5*time.Second))
	require.NoError(t, c.RemoveContainer(ctx, id, true))
}
```

- [ ] **Step 2: 통합 테스트 실행 (dev 머신에서)**

Docker daemon이 동작 중이어야 함.

```bash
go test -tags integration ./internal/envdocker/ -race -count=1 -v -timeout 90s
```
Expected: 1 PASS.

CI에서는:
```bash
go test ./internal/envdocker/ -race -count=1
```
Expected: 3 PASS (integration 빌드 태그 없이는 integration_test.go 안 컴파일됨).

- [ ] **Step 3: Commit**

```bash
git add internal/envdocker/integration_test.go
git commit -m "feat(envdocker): real Docker integration test under //go:build integration"
```

---

### Task 9: envlifecycle — GPU Allocator + 라벨 복원

**Files:**
- Create: `internal/envlifecycle/allocator.go`
- Create: `internal/envlifecycle/allocator_test.go`

- [ ] **Step 1: 실패 테스트**

`internal/envlifecycle/allocator_test.go`:
```go
package envlifecycle_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
)

func TestAllocator_AllocateZero(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	idx, err := a.Allocate(uuid.New(), 0)
	require.NoError(t, err)
	require.Empty(t, idx)
}

func TestAllocator_AllocateSequential(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1, 2, 3})
	e1, e2 := uuid.New(), uuid.New()

	i1, err := a.Allocate(e1, 2)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, i1)

	i2, err := a.Allocate(e2, 2)
	require.NoError(t, err)
	require.Equal(t, []int{2, 3}, i2)
}

func TestAllocator_Insufficient(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	_, err := a.Allocate(uuid.New(), 3)
	require.ErrorIs(t, err, envlifecycle.ErrInsufficientGPUs)
}

func TestAllocator_Release(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	e1 := uuid.New()
	_, _ = a.Allocate(e1, 2)
	a.Release(e1)

	e2 := uuid.New()
	idx, err := a.Allocate(e2, 2)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, idx)
}

func TestAllocator_RestoreFromDocker(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1, 2, 3})
	mock := envdocker.NewMockDockerClient()
	ctx := context.Background()
	envID := uuid.New().String()
	_, _ = mock.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-x",
		Labels: map[string]string{
			envdocker.LabelEnvID:      envID,
			envdocker.LabelRole:       "dev",
			envdocker.LabelGPUIndices: "1,3",
		},
	})

	require.NoError(t, a.RestoreFromDocker(ctx, mock))

	// 0과 2만 free
	idx, err := a.Allocate(uuid.New(), 2)
	require.NoError(t, err)
	require.Equal(t, []int{0, 2}, idx)
}

func TestAllocator_RestoreFromDocker_EmptyIndices(t *testing.T) {
	a := envlifecycle.NewGPUAllocator([]int{0, 1})
	mock := envdocker.NewMockDockerClient()
	ctx := context.Background()
	_, _ = mock.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: "flex-env-x",
		Labels: map[string]string{
			envdocker.LabelEnvID:      uuid.New().String(),
			envdocker.LabelRole:       "dev",
			envdocker.LabelGPUIndices: "",  // 0 GPU env
		},
	})
	require.NoError(t, a.RestoreFromDocker(ctx, mock))

	// 0, 1 둘 다 free
	idx, _ := a.Allocate(uuid.New(), 2)
	require.Equal(t, []int{0, 1}, idx)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/envlifecycle/ -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/envlifecycle/allocator.go`:
```go
package envlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/envdocker"
)

var ErrInsufficientGPUs = errors.New("insufficient GPUs")

type GPUAllocator struct {
	mu        sync.Mutex
	total     []int
	allocated map[uuid.UUID][]int
}

func NewGPUAllocator(totalIndices []int) *GPUAllocator {
	cp := append([]int(nil), totalIndices...)
	sort.Ints(cp)
	return &GPUAllocator{total: cp, allocated: map[uuid.UUID][]int{}}
}

// Allocate reserves `count` indices for envID. Returns the chosen indices (sorted).
// count=0 returns an empty slice and reserves nothing.
func (a *GPUAllocator) Allocate(envID uuid.UUID, count int) ([]int, error) {
	a.mu.Lock(); defer a.mu.Unlock()
	if count == 0 {
		a.allocated[envID] = nil
		return []int{}, nil
	}
	used := map[int]bool{}
	for _, idxs := range a.allocated {
		for _, i := range idxs { used[i] = true }
	}
	var free []int
	for _, i := range a.total {
		if !used[i] { free = append(free, i) }
	}
	if len(free) < count {
		return nil, fmt.Errorf("%w: requested %d, available %d", ErrInsufficientGPUs, count, len(free))
	}
	pick := append([]int(nil), free[:count]...)
	a.allocated[envID] = pick
	return pick, nil
}

func (a *GPUAllocator) Release(envID uuid.UUID) {
	a.mu.Lock(); defer a.mu.Unlock()
	delete(a.allocated, envID)
}

// RestoreFromDocker walks the dev containers and rebuilds the allocation map
// from flexctl.gpu_indices labels. Called once at agent startup.
func (a *GPUAllocator) RestoreFromDocker(ctx context.Context, c envdocker.DockerClient) error {
	list, err := c.ListContainers(ctx, map[string]string{
		envdocker.LabelRole: "dev",
	})
	if err != nil { return fmt.Errorf("list: %w", err) }

	a.mu.Lock(); defer a.mu.Unlock()
	for _, info := range list {
		envIDStr := info.Labels[envdocker.LabelEnvID]
		if envIDStr == "" { continue }
		envID, err := uuid.Parse(envIDStr)
		if err != nil { continue }
		idxStr := info.Labels[envdocker.LabelGPUIndices]
		indices := parseIndicesCSV(idxStr)
		a.allocated[envID] = indices
	}
	return nil
}

// Restored returns the allocation map (test helper).
func (a *GPUAllocator) Restored() map[uuid.UUID][]int {
	a.mu.Lock(); defer a.mu.Unlock()
	out := make(map[uuid.UUID][]int, len(a.allocated))
	for k, v := range a.allocated {
		out[k] = append([]int(nil), v...)
	}
	return out
}

func parseIndicesCSV(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" { return nil }
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// IndicesCSV formats indices for a container label.
func IndicesCSV(idx []int) string {
	parts := make([]string, 0, len(idx))
	for _, i := range idx { parts = append(parts, strconv.Itoa(i)) }
	return strings.Join(parts, ",")
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/envlifecycle/ -race -count=1 -v`
Expected: 6 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/envlifecycle/allocator.go internal/envlifecycle/allocator_test.go
git commit -m "feat(envlifecycle): GPU allocator with label-based docker restore"
```

---

### Task 10: envlifecycle dispatcher — CreateEnv 흐름

**Files:**
- Create: `internal/envlifecycle/dispatcher.go`
- Create: `internal/envlifecycle/dispatcher_test.go`

이 task는 CreateEnv 명령 수신 → 사이드카 + dev 컨테이너 묶음 생성 → EnvReady 전송하는 핵심 로직. Stop/Start/Delete는 Task 11.

- [ ] **Step 1: 실패 테스트**

`internal/envlifecycle/dispatcher_test.go`:
```go
package envlifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
)

// stubStream collects AgentMessages sent by the dispatcher.
type stubStream struct {
	sent []*agentpb.AgentMessage
}

func (s *stubStream) Send(msg *agentpb.AgentMessage) error {
	s.sent = append(s.sent, proto.Clone(msg).(*agentpb.AgentMessage))
	return nil
}

func newDispatcher(t *testing.T, m envdocker.DockerClient, sender envlifecycle.AgentSender) *envlifecycle.Dispatcher {
	t.Helper()
	alloc := envlifecycle.NewGPUAllocator([]int{0, 1, 2, 3})
	return envlifecycle.NewDispatcher(m, alloc, sender)
}

func TestDispatcher_CreateEnv_HappyPath(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	cmd := &agentpb.CreateEnv{
		EnvId:           envID.String(),
		ImageRef:        "flex/dev-cuda-base:dev",
		SidecarImageRef: "flex/sidecar:dev",
		Hostname:        "paul-vllm",
		HeadscaleUrl:    "http://headscale:8080",
		PreauthKey:      "ts-key-1",
		Tags:            []string{"tag:env-paul"},
		AuthorizedKeys:  "ssh-ed25519 AAAA...",
		GpuRequest:      2,
	}

	// Pre-arm sidecar health check (Exec returns "100.x.x.x" → online)
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = "tailscale 100.64.0.5\n"

	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	// Verify two containers created
	got, _ := mock.ListContainers(context.Background(), nil)
	require.Len(t, got, 2)

	// EnvReady sent
	require.Len(t, stub.sent, 1)
	ack := stub.sent[0].GetEnvReady()
	require.NotNil(t, ack)
	require.Equal(t, envID.String(), ack.EnvId)
	require.Equal(t, []int32{0, 1}, ack.GpuIndices)
}

func TestDispatcher_CreateEnv_InsufficientGPU(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	alloc := envlifecycle.NewGPUAllocator([]int{0})  // 1 GPU only
	d := envlifecycle.NewDispatcher(mock, alloc, stub)

	envID := uuid.New()
	cmd := &agentpb.CreateEnv{
		EnvId:           envID.String(),
		ImageRef:        "flex/dev-cuda-base:dev",
		SidecarImageRef: "flex/sidecar:dev",
		Hostname:        "paul-x",
		PreauthKey:      "ts-key-2",
		Tags:            []string{"tag:env-paul"},
		GpuRequest:      2,  // exceeds available
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	// No container created
	got, _ := mock.ListContainers(context.Background(), nil)
	require.Empty(t, got)

	// EnvError sent
	require.Len(t, stub.sent, 1)
	errMsg := stub.sent[0].GetEnvError()
	require.NotNil(t, errMsg)
	require.Equal(t, "unknown", errMsg.Stage)
	require.Contains(t, errMsg.Detail, "insufficient")
}

func TestDispatcher_CreateEnv_SidecarStartFails_CleansUp(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	envID := uuid.New()
	mock.NextErrors["StartContainer:flex-net-"+envID.String()+"-id"] = errors.New("boom")

	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	cmd := &agentpb.CreateEnv{
		EnvId: envID.String(), ImageRef: "x", SidecarImageRef: "y",
		Hostname: "h", PreauthKey: "k", GpuRequest: 1,
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	// 사이드카 create했다가 start 실패 후 remove 호출
	require.Contains(t, mock.Calls, "RemoveContainer:flex-net-"+envID.String()+"-id")

	// EnvError sent with stage "sidecar_start"
	require.Len(t, stub.sent, 1)
	errMsg := stub.sent[0].GetEnvError()
	require.NotNil(t, errMsg)
	require.Equal(t, "sidecar_start", errMsg.Stage)
}

func TestDispatcher_CreateEnv_SidecarHealthTimeout(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	alloc := envlifecycle.NewGPUAllocator([]int{0, 1})
	d := envlifecycle.NewDispatcherWithTimeout(mock, alloc, stub, 200*time.Millisecond)

	envID := uuid.New()
	// Exec returns empty → never online
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = ""

	cmd := &agentpb.CreateEnv{
		EnvId: envID.String(), ImageRef: "x", SidecarImageRef: "y",
		Hostname: "h", PreauthKey: "k", GpuRequest: 1,
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))

	require.Len(t, stub.sent, 1)
	errMsg := stub.sent[0].GetEnvError()
	require.NotNil(t, errMsg)
	require.Equal(t, "sidecar_health", errMsg.Stage)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/envlifecycle/ -v -run TestDispatcher_CreateEnv`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/envlifecycle/dispatcher.go`:
```go
package envlifecycle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/envdocker"
)

// AgentSender abstracts the agent → control-plane stream send so the dispatcher
// can be tested without a real gRPC stream.
type AgentSender interface {
	Send(msg *agentpb.AgentMessage) error
}

type Dispatcher struct {
	docker       envdocker.DockerClient
	alloc        *GPUAllocator
	sender       AgentSender
	healthTimeout time.Duration
}

func NewDispatcher(docker envdocker.DockerClient, alloc *GPUAllocator, sender AgentSender) *Dispatcher {
	return NewDispatcherWithTimeout(docker, alloc, sender, 30*time.Second)
}

func NewDispatcherWithTimeout(docker envdocker.DockerClient, alloc *GPUAllocator, sender AgentSender, healthTimeout time.Duration) *Dispatcher {
	return &Dispatcher{docker: docker, alloc: alloc, sender: sender, healthTimeout: healthTimeout}
}

func (d *Dispatcher) sendError(envID, stage, detail string) {
	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvError{
			EnvError: &agentpb.EnvError{EnvId: envID, Stage: stage, Detail: detail},
		},
	})
}

func (d *Dispatcher) sendReady(envID, sidecarID, devID string, gpuIdx []int) {
	idx32 := make([]int32, 0, len(gpuIdx))
	for _, i := range gpuIdx { idx32 = append(idx32, int32(i)) }
	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvReady{
			EnvReady: &agentpb.EnvReady{
				EnvId: envID, SidecarContainerId: sidecarID, DevContainerId: devID, GpuIndices: idx32,
			},
		},
	})
}

func (d *Dispatcher) HandleCreate(ctx context.Context, cmd *agentpb.CreateEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id: "+err.Error())
		return nil
	}

	// 1) Allocate GPU
	indices, err := d.alloc.Allocate(envID, int(cmd.GetGpuRequest()))
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", err.Error())
		return nil
	}

	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()

	// 2) Create sidecar
	sidecarLabels := map[string]string{
		envdocker.LabelEnvID:        cmd.GetEnvId(),
		envdocker.LabelRole:         "sidecar",
		envdocker.LabelHostname:     cmd.GetHostname(),
		envdocker.LabelHeadscaleURL: cmd.GetHeadscaleUrl(),
		envdocker.LabelTags:         strings.Join(cmd.GetTags(), ","),
		envdocker.LabelImageRef:     cmd.GetSidecarImageRef(),
	}
	sidecarEnv := map[string]string{
		"FLEXCTL_HOSTNAME":     cmd.GetHostname(),
		"FLEXCTL_AUTHKEY":      cmd.GetPreauthKey(),
		"FLEXCTL_HEADSCALE_URL": cmd.GetHeadscaleUrl(),
		"FLEXCTL_TAGS":         strings.Join(cmd.GetTags(), ","),
	}
	sidecarID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: sidecarName, Image: cmd.GetSidecarImageRef(),
		Env: sidecarEnv, Labels: sidecarLabels,
		CapAdd:  []string{"NET_ADMIN"},
		Devices: []string{"/dev/net/tun"},
	})
	if err != nil {
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "sidecar_start", err.Error())
		return nil
	}

	// 3) Start sidecar
	if err := d.docker.StartContainer(ctx, sidecarID); err != nil {
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "sidecar_start", err.Error())
		return nil
	}

	// 4) Wait for sidecar health (poll tailscale status)
	if err := d.waitSidecarHealth(ctx, sidecarID); err != nil {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "sidecar_health", err.Error())
		return nil
	}

	// 5) Create dev container sharing sidecar's netns
	devLabels := map[string]string{
		envdocker.LabelEnvID:      cmd.GetEnvId(),
		envdocker.LabelRole:       "dev",
		envdocker.LabelImageRef:   cmd.GetImageRef(),
		envdocker.LabelGPUIndices: IndicesCSV(indices),
		envdocker.LabelVolumeName: "flex-env-" + cmd.GetEnvId(),
	}
	devEnv := map[string]string{
		"FLEXCTL_AUTHORIZED_KEYS": cmd.GetAuthorizedKeys(),
	}
	devID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: devName, Image: cmd.GetImageRef(), Cmd: cmd.GetDefaultCmd(),
		Env: devEnv, Labels: devLabels,
		NetworkMode: "container:" + sidecarName,
		GPUIndices:  indices,
		VolumeMounts: []envdocker.VolumeMount{
			{Source: "flex-env-" + cmd.GetEnvId(), Target: "/home/dev"},
		},
	})
	if err != nil {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "dev_start", err.Error())
		return nil
	}

	if err := d.docker.StartContainer(ctx, devID); err != nil {
		_ = d.docker.RemoveContainer(ctx, devID, true)
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "dev_start", err.Error())
		return nil
	}

	d.sendReady(cmd.GetEnvId(), sidecarID, devID, indices)
	return nil
}

// waitSidecarHealth polls `tailscale status` until it reports an IP, or times out.
// We consider any non-empty stdout containing "100." (tailnet CGNAT prefix) as healthy.
func (d *Dispatcher) waitSidecarHealth(ctx context.Context, sidecarID string) error {
	deadline := time.Now().Add(d.healthTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		stdout, _, _, err := d.docker.Exec(ctx, sidecarID, []string{"tailscale", "status"})
		if err == nil {
			buf, _ := io.ReadAll(stdout)
			if strings.Contains(string(buf), "100.") {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("sidecar did not report tailnet IP in %s", d.healthTimeout)
}

// no-op for slog imports until Task 11 expands this file
var _ = slog.Default
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/envlifecycle/ -race -count=1 -v`
Expected: 모든 allocator (6) + dispatcher CreateEnv (4) = 10 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/envlifecycle/dispatcher.go internal/envlifecycle/dispatcher_test.go
git commit -m "feat(envlifecycle): CreateEnv dispatcher with sidecar+dev orchestration"
```

---

### Task 11: envlifecycle dispatcher — Stop / Start / Delete + error stages

**Files:**
- Modify: `internal/envlifecycle/dispatcher.go`
- Modify: `internal/envlifecycle/dispatcher_test.go`

이 task는 `HandleStop`, `HandleStart`, `HandleDelete` 메서드 + 각각의 테스트.

- [ ] **Step 1: 실패 테스트 추가**

`internal/envlifecycle/dispatcher_test.go` 끝에 추가:
```go
// helper: create env via HandleCreate to set up state
func setupRunningEnv(t *testing.T, d *envlifecycle.Dispatcher, mock *envdocker.MockDockerClient, envID uuid.UUID) {
	t.Helper()
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = "tailscale 100.64.0.5\n"
	cmd := &agentpb.CreateEnv{
		EnvId: envID.String(), ImageRef: "flex/dev:dev", SidecarImageRef: "flex/sidecar:dev",
		Hostname: "h", PreauthKey: "k", GpuRequest: 1,
	}
	require.NoError(t, d.HandleCreate(context.Background(), cmd))
}

func TestDispatcher_StopEnv(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	stub.sent = nil // reset

	require.NoError(t, d.HandleStop(context.Background(), &agentpb.StopEnv{EnvId: envID.String()}))

	require.Len(t, stub.sent, 1)
	require.NotNil(t, stub.sent[0].GetEnvStopped())
	require.Equal(t, envID.String(), stub.sent[0].GetEnvStopped().EnvId)

	// 둘 다 exited 상태
	infoDev, _ := mock.InspectContainer(context.Background(), "flex-env-"+envID.String()+"-id")
	require.Equal(t, "exited", infoDev.State)
}

func TestDispatcher_StartEnv(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	require.NoError(t, d.HandleStop(context.Background(), &agentpb.StopEnv{EnvId: envID.String()}))
	stub.sent = nil

	// Re-arm health for restart's recreated sidecar
	mock.ExecOutput["flex-net-"+envID.String()+"-id"] = "tailscale 100.64.0.5\n"
	require.NoError(t, d.HandleStart(context.Background(), &agentpb.StartEnv{
		EnvId: envID.String(), PreauthKey: "new-key", AuthorizedKeys: "ssh-ed25519 new",
	}))

	require.Len(t, stub.sent, 1)
	require.NotNil(t, stub.sent[0].GetEnvReady())
}

func TestDispatcher_DeleteEnv(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	stub.sent = nil

	require.NoError(t, d.HandleDelete(context.Background(), &agentpb.DeleteEnv{EnvId: envID.String()}))

	require.Len(t, stub.sent, 1)
	require.NotNil(t, stub.sent[0].GetEnvDeleted())

	// 컨테이너 둘 다 사라짐
	all, _ := mock.ListContainers(context.Background(), nil)
	require.Empty(t, all)
}

func TestDispatcher_DeleteEnv_VolumeRemoved(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	mock.Volumes["flex-env-pre"] = true
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)
	mock.Volumes["flex-env-"+envID.String()] = true
	stub.sent = nil

	require.NoError(t, d.HandleDelete(context.Background(), &agentpb.DeleteEnv{EnvId: envID.String()}))

	require.False(t, mock.Volumes["flex-env-"+envID.String()])
	require.True(t, mock.Volumes["flex-env-pre"]) // 다른 env 영향 없음
}

func TestDispatcher_BuildEnvStateSnapshot(t *testing.T) {
	mock := envdocker.NewMockDockerClient()
	stub := &stubStream{}
	d := newDispatcher(t, mock, stub)

	envID := uuid.New()
	setupRunningEnv(t, d, mock, envID)

	snap, err := d.BuildEnvStateSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{envID.String()}, snap)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/envlifecycle/ -run "TestDispatcher_(Stop|Start|Delete|BuildEnvStateSnapshot)" -v`
Expected: 컴파일 에러 (`HandleStop`, `HandleStart`, `HandleDelete`, `BuildEnvStateSnapshot` 미정의).

- [ ] **Step 3: 구현 — dispatcher.go에 메서드 추가**

`internal/envlifecycle/dispatcher.go` 끝에 추가:
```go
func (d *Dispatcher) HandleStop(ctx context.Context, cmd *agentpb.StopEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id")
		return nil
	}
	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()
	if err := d.docker.StopContainer(ctx, devName+"-id", 10*time.Second); err != nil {
		slog.Warn("stop dev", "err", err)
	}
	if err := d.docker.StopContainer(ctx, sidecarName+"-id", 10*time.Second); err != nil {
		slog.Warn("stop sidecar", "err", err)
	}
	d.alloc.Release(envID)
	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvStopped{
			EnvStopped: &agentpb.EnvStopped{EnvId: cmd.GetEnvId()},
		},
	})
	return nil
}

func (d *Dispatcher) HandleStart(ctx context.Context, cmd *agentpb.StartEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id")
		return nil
	}
	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()

	// Inspect existing dev container to recover labels (image_ref, gpu_indices, etc.)
	devInfo, err := d.docker.InspectContainer(ctx, devName+"-id")
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "dev container not found: "+err.Error())
		return nil
	}
	gpuIndices := parseIndicesCSV(devInfo.Labels[envdocker.LabelGPUIndices])

	sidecarInfo, err := d.docker.InspectContainer(ctx, sidecarName+"-id")
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "sidecar container not found: "+err.Error())
		return nil
	}

	// Reallocate GPU
	if len(gpuIndices) > 0 {
		if _, err := d.alloc.Allocate(envID, len(gpuIndices)); err != nil {
			d.sendError(cmd.GetEnvId(), "start", err.Error())
			return nil
		}
	}

	// Recreate sidecar with new preauth_key
	if err := d.docker.RemoveContainer(ctx, sidecarName+"-id", true); err != nil {
		d.sendError(cmd.GetEnvId(), "start", "rm sidecar: "+err.Error())
		d.alloc.Release(envID)
		return nil
	}
	sidecarID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name:    sidecarName,
		Image:   sidecarInfo.Labels[envdocker.LabelImageRef],
		Labels:  sidecarInfo.Labels,
		CapAdd:  []string{"NET_ADMIN"},
		Devices: []string{"/dev/net/tun"},
		Env: map[string]string{
			"FLEXCTL_HOSTNAME":      sidecarInfo.Labels[envdocker.LabelHostname],
			"FLEXCTL_AUTHKEY":       cmd.GetPreauthKey(),
			"FLEXCTL_HEADSCALE_URL": sidecarInfo.Labels[envdocker.LabelHeadscaleURL],
			"FLEXCTL_TAGS":          sidecarInfo.Labels[envdocker.LabelTags],
		},
	})
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "create sidecar: "+err.Error())
		d.alloc.Release(envID)
		return nil
	}
	if err := d.docker.StartContainer(ctx, sidecarID); err != nil {
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "start", "start sidecar: "+err.Error())
		return nil
	}
	if err := d.waitSidecarHealth(ctx, sidecarID); err != nil {
		_ = d.docker.StopContainer(ctx, sidecarID, 5*time.Second)
		_ = d.docker.RemoveContainer(ctx, sidecarID, true)
		d.alloc.Release(envID)
		d.sendError(cmd.GetEnvId(), "start", "sidecar health: "+err.Error())
		return nil
	}

	// Recreate dev with new authorized_keys
	if err := d.docker.RemoveContainer(ctx, devName+"-id", true); err != nil {
		d.sendError(cmd.GetEnvId(), "start", "rm dev: "+err.Error())
		return nil
	}
	devID, err := d.docker.CreateContainer(ctx, envdocker.ContainerSpec{
		Name: devName, Image: devInfo.Labels[envdocker.LabelImageRef],
		Labels:      devInfo.Labels,
		NetworkMode: "container:" + sidecarName,
		GPUIndices:  gpuIndices,
		Env:         map[string]string{"FLEXCTL_AUTHORIZED_KEYS": cmd.GetAuthorizedKeys()},
		VolumeMounts: []envdocker.VolumeMount{
			{Source: devInfo.Labels[envdocker.LabelVolumeName], Target: "/home/dev"},
		},
	})
	if err != nil {
		d.sendError(cmd.GetEnvId(), "start", "create dev: "+err.Error())
		return nil
	}
	if err := d.docker.StartContainer(ctx, devID); err != nil {
		d.sendError(cmd.GetEnvId(), "start", "start dev: "+err.Error())
		return nil
	}

	d.sendReady(cmd.GetEnvId(), sidecarID, devID, gpuIndices)
	return nil
}

func (d *Dispatcher) HandleDelete(ctx context.Context, cmd *agentpb.DeleteEnv) error {
	envID, err := uuid.Parse(cmd.GetEnvId())
	if err != nil {
		d.sendError(cmd.GetEnvId(), "unknown", "invalid env_id")
		return nil
	}
	sidecarName := "flex-net-" + cmd.GetEnvId()
	devName := "flex-env-" + cmd.GetEnvId()
	volumeName := "flex-env-" + cmd.GetEnvId()

	_ = d.docker.StopContainer(ctx, devName+"-id", 5*time.Second)
	_ = d.docker.StopContainer(ctx, sidecarName+"-id", 5*time.Second)
	_ = d.docker.RemoveContainer(ctx, devName+"-id", true)
	_ = d.docker.RemoveContainer(ctx, sidecarName+"-id", true)
	if err := d.docker.RemoveVolume(ctx, volumeName); err != nil {
		slog.Warn("rm volume", "err", err, "name", volumeName)
	}
	d.alloc.Release(envID)

	_ = d.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvDeleted{
			EnvDeleted: &agentpb.EnvDeleted{EnvId: cmd.GetEnvId()},
		},
	})
	return nil
}

// BuildEnvStateSnapshot lists running dev containers and returns their env_ids.
// Called by flexctlagent on reconnect (sent as EnvStateSnapshot).
func (d *Dispatcher) BuildEnvStateSnapshot(ctx context.Context) ([]string, error) {
	list, err := d.docker.ListContainers(ctx, map[string]string{
		envdocker.LabelRole: "dev",
	})
	if err != nil { return nil, err }
	var out []string
	for _, c := range list {
		if c.State != "running" { continue }
		if id := c.Labels[envdocker.LabelEnvID]; id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/envlifecycle/ -race -count=1 -v`
Expected: 모든 테스트 PASS (allocator + dispatcher Create + Stop + Start + Delete + Snapshot).

NOTE: Stop 테스트에서 dev 컨테이너의 State 가 "exited"여야 — MockDockerClient.StopContainer가 State를 "exited"로 마킹함을 Task 7에서 확인.

- [ ] **Step 5: Commit**

```bash
git add internal/envlifecycle/dispatcher.go internal/envlifecycle/dispatcher_test.go
git commit -m "feat(envlifecycle): Stop/Start/Delete handlers + EnvStateSnapshot builder"
```

---

### Task 12: agentstream 확장 + flexctlagent 통합

**Files:**
- Modify: `internal/agentstream/server.go` (env 명령 dispatch + ack 처리)
- Modify: `internal/agentstream/server_test.go` (확장)
- Modify: `internal/flexctlagent/agent.go` (ControlMessage 라우팅 + EnvStateSnapshot 전송)
- Modify: `cmd/control-plane/main.go` (noopEnvDispatcher 제거, 진짜 구현으로 교체)

이번 task는 양쪽(서버+클라이언트)을 한 번에 와이어링. envs.Dispatcher 인터페이스의 진짜 구현이 agentstream에 들어감.

#### 서버 측 (agentstream)

- [ ] **Step 1: agentstream에 envs.Dispatcher 구현 추가**

`internal/agentstream/server.go` 수정 — `Server` 구조체에 ack 채널 + envs 서비스 의존성 추가:

```go
import (
	// ... 기존 imports
	"sync"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/users"
	"github.com/paul/flexctl/internal/sshkeys"
)
```



기존 `Server` 구조체를 다음으로 교체:
```go
type Server struct {
	agentpb.UnimplementedAgentServer

	nodes  *nodes.Service
	envs   *envs.Service
	users  *users.Service
	keys   *sshkeys.Service
	hs     *headscale.Client

	// node_id → live stream (envs Dispatcher가 명령을 push할 때 사용)
	mu      sync.Mutex
	streams map[uuid.UUID]agentpb.Agent_StreamServer
}

func NewServer(nodesSvc *nodes.Service, envsSvc *envs.Service, usersSvc *users.Service,
	keysSvc *sshkeys.Service, hs *headscale.Client) *Server {
	return &Server{
		nodes:   nodesSvc, envs: envsSvc, users: usersSvc, keys: keysSvc, hs: hs,
		streams: map[uuid.UUID]agentpb.Agent_StreamServer{},
	}
}

func (s *Server) registerStream(nodeID uuid.UUID, stream agentpb.Agent_StreamServer) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.streams[nodeID] = stream
}

func (s *Server) unregisterStream(nodeID uuid.UUID) {
	s.mu.Lock(); defer s.mu.Unlock()
	delete(s.streams, nodeID)
}

func (s *Server) streamFor(nodeID uuid.UUID) (agentpb.Agent_StreamServer, bool) {
	s.mu.Lock(); defer s.mu.Unlock()
	st, ok := s.streams[nodeID]
	return st, ok
}
```

기존 `Stream` 메서드 수정 — auth 성공 후 stream 등록, defer 해제, env 메시지 처리 추가:

```go
func (s *Server) Stream(stream agentpb.Agent_StreamServer) error {
	ctx := stream.Context()
	// ... 기존 auth 로직 (Plan 3) ...
	// node, err := s.nodes.AuthenticateNodeToken(...)

	// NEW: stream 등록
	s.registerStream(node.ID, stream)
	defer s.unregisterStream(node.ID)

	// 기존: register 받기 + ack
	// ... 기존 코드 ...

	// 메인 루프: 모든 AgentMessage payload 분기
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) { return nil }
		if err != nil { return err }
		switch p := msg.GetPayload().(type) {
		case *agentpb.AgentMessage_Heartbeat:
			if err := s.nodes.RecordHeartbeat(ctx, node.ID); err != nil {
				slog.Warn("record heartbeat", "err", err)
			}
			_ = stream.Send(&agentpb.ControlMessage{
				Payload: &agentpb.ControlMessage_HeartbeatAck{HeartbeatAck: &agentpb.HeartbeatAck{}},
			})
		case *agentpb.AgentMessage_EnvReady:
			r := p.EnvReady
			envID, _ := uuid.Parse(r.GetEnvId())
			gpus := make([]int, 0, len(r.GetGpuIndices()))
			for _, g := range r.GetGpuIndices() { gpus = append(gpus, int(g)) }
			if err := s.envs.MarkRunning(ctx, envID, r.GetSidecarContainerId(), r.GetDevContainerId(), gpus); err != nil {
				slog.Error("envs.MarkRunning", "err", err, "env_id", r.GetEnvId())
			}
		case *agentpb.AgentMessage_EnvStopped:
			envID, _ := uuid.Parse(p.EnvStopped.GetEnvId())
			if err := s.envs.MarkStopped(ctx, envID); err != nil {
				slog.Error("envs.MarkStopped", "err", err)
			}
		case *agentpb.AgentMessage_EnvDeleted:
			envID, _ := uuid.Parse(p.EnvDeleted.GetEnvId())
			if err := s.envs.Delete(ctx, envID); err != nil {
				slog.Error("envs.Delete", "err", err)
			}
		case *agentpb.AgentMessage_EnvError:
			envID, _ := uuid.Parse(p.EnvError.GetEnvId())
			if err := s.envs.MarkError(ctx, envID, p.EnvError.GetStage(), p.EnvError.GetDetail()); err != nil {
				slog.Error("envs.MarkError", "err", err)
			}
		case *agentpb.AgentMessage_EnvSnapshot:
			// shallow resync: DB.running ∖ agent.report → mark error
			reportedSet := map[string]bool{}
			for _, id := range p.EnvSnapshot.GetRunningEnvIds() { reportedSet[id] = true }
			dbRunning, err := s.envs.ListRunningOnNode(ctx, node.ID)
			if err != nil { slog.Error("list running", "err", err); continue }
			for _, e := range dbRunning {
				if !reportedSet[e.ID.String()] {
					if err := s.envs.MarkError(ctx, e.ID, "resync", "lost during agent reconnect"); err != nil {
						slog.Error("mark error on resync", "err", err)
					}
				}
			}
		default:
			slog.Warn("unexpected message type from agent", "node_id", node.ID.String())
		}
	}
}

// EnvsDispatcher implements envs.Dispatcher via the agent gRPC stream.
type EnvsDispatcher struct {
	srv   *Server
	envs  *envs.Service
	users *users.Service
	keys  *sshkeys.Service
	hs    *headscale.Client

	// settings
	headscaleClientURL string  // URL agent uses to reach Headscale
	sidecarImage       string  // "flex/sidecar:dev"
}

func NewEnvsDispatcher(srv *Server, headscaleClientURL, sidecarImage string) *EnvsDispatcher {
	return &EnvsDispatcher{
		srv: srv, envs: srv.envs, users: srv.users, keys: srv.keys, hs: srv.hs,
		headscaleClientURL: headscaleClientURL, sidecarImage: sidecarImage,
	}
}

func (d *EnvsDispatcher) Create(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil { return err }
	user, err := d.users.ByID(ctx, env.OwnerUserID)
	if err != nil { return err }
	keys, err := d.keys.List(ctx, env.OwnerUserID)
	if err != nil { return err }
	keyText := concatKeys(keys)
	pak, err := d.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: user.Slug, Ephemeral: true, Reusable: false,
		Expiration: 24 * time.Hour, ACLTags: []string{"tag:env-" + user.Slug},
	})
	if err != nil { return fmt.Errorf("preauthkey: %w", err) }

	tpl, err := d.envs.TemplateByID(ctx, env.TemplateID)  // 헬퍼 추가 필요 — service.go에 add
	if err != nil { return err }

	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok { return fmt.Errorf("agent not connected for node %s", env.NodeID) }

	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_CreateEnv{
			CreateEnv: &agentpb.CreateEnv{
				EnvId: envID.String(),
				ImageRef: tpl.ImageRef, SidecarImageRef: d.sidecarImage,
				Hostname: env.Hostname, HeadscaleUrl: d.headscaleClientURL,
				PreauthKey: pak.Key, Tags: []string{"tag:env-" + user.Slug},
				AuthorizedKeys: keyText, GpuRequest: env.GPURequest,
				DefaultCmd: tpl.DefaultCmd,
			},
		},
	})
}

func (d *EnvsDispatcher) Stop(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil { return err }
	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok { return fmt.Errorf("agent not connected") }
	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_StopEnv{StopEnv: &agentpb.StopEnv{EnvId: envID.String()}},
	})
}

func (d *EnvsDispatcher) Start(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil { return err }
	user, err := d.users.ByID(ctx, env.OwnerUserID)
	if err != nil { return err }
	keys, err := d.keys.List(ctx, env.OwnerUserID)
	if err != nil { return err }
	pak, err := d.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User: user.Slug, Ephemeral: true, Reusable: false,
		Expiration: 24 * time.Hour, ACLTags: []string{"tag:env-" + user.Slug},
	})
	if err != nil { return err }
	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok { return fmt.Errorf("agent not connected") }
	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_StartEnv{
			StartEnv: &agentpb.StartEnv{
				EnvId: envID.String(), PreauthKey: pak.Key,
				AuthorizedKeys: concatKeys(keys),
			},
		},
	})
}

func (d *EnvsDispatcher) Delete(ctx context.Context, envID uuid.UUID) error {
	env, err := d.envs.ByID(ctx, envID)
	if err != nil { return err }
	stream, ok := d.srv.streamFor(env.NodeID)
	if !ok {
		// agent offline → just remove DB row, agent will rediscover on reconnect (resync deletes)
		return d.envs.Delete(ctx, envID)
	}
	return stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_DeleteEnv{DeleteEnv: &agentpb.DeleteEnv{EnvId: envID.String()}},
	})
}

func concatKeys(keys []sshkeys.Key) string {
	var b strings.Builder
	for _, k := range keys { b.WriteString(k.PublicKey); b.WriteString("\n") }
	return b.String()
}
```

#### envs.Service에 TemplateByID helper 추가

`internal/envs/service.go` 끝에 추가 (간소화 위해 envs.Service가 templates 조회도 위임):
```go
// TemplateByID is a thin pass-through used by the dispatcher.
// 실제로는 imagetemplates.Service를 주입받는 것이 더 깨끗하지만 MVP에선 같은 pool 사용.
func (s *Service) TemplateByID(ctx context.Context, id string) (TemplateInfo, error) {
	var t TemplateInfo
	err := s.pool.QueryRow(ctx,
		`SELECT image_ref, default_cmd FROM image_templates WHERE id = $1 AND enabled = true`, id,
	).Scan(&t.ImageRef, &t.DefaultCmd)
	if errors.Is(err, pgx.ErrNoRows) { return TemplateInfo{}, ErrNotFound }
	if err != nil { return TemplateInfo{}, fmt.Errorf("template: %w", err) }
	return t, nil
}

type TemplateInfo struct {
	ImageRef   string
	DefaultCmd []string
}
```

#### 클라이언트 측 (flexctlagent)

- [ ] **Step 2: flexctlagent.go 수정 — ControlMessage 라우팅**

`internal/flexctlagent/agent.go`의 `runOnce` 메서드를 찾아, RegisterAck 받은 다음 부분에 EnvStateSnapshot 전송 + ControlMessage 분기 추가.

기존 Recv 루프(스트림 메시지 수신 goroutine)를 다음으로 교체:
```go
// dispatcher field 추가 (이미 있는 Agent struct에)
type Agent struct {
	cfg        Config
	dispatcher *envlifecycle.Dispatcher  // NEW
}

// NewWithDispatcher constructor 추가
func NewWithDispatcher(cfg Config, disp *envlifecycle.Dispatcher) *Agent {
	a := New(cfg)
	a.dispatcher = disp
	return a
}
```

`runOnce` 안에서 RegisterAck 받은 직후:
```go
// NEW: send EnvStateSnapshot
if a.dispatcher != nil {
	envIDs, err := a.dispatcher.BuildEnvStateSnapshot(ctx)
	if err == nil {
		_ = stream.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_EnvSnapshot{
				EnvSnapshot: &agentpb.EnvStateSnapshot{RunningEnvIds: envIDs},
			},
		})
	}
}
```

기존 ack 수신 goroutine (`for { stream.Recv(); ... }`) 안의 default 처리를 다음으로 확장:
```go
go func() {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) { errCh <- nil } else { errCh <- err }
			return
		}
		// NEW: ControlMessage 분기
		if a.dispatcher != nil {
			switch p := msg.GetPayload().(type) {
			case *agentpb.ControlMessage_CreateEnv:
				go a.dispatcher.HandleCreate(context.Background(), p.CreateEnv)
			case *agentpb.ControlMessage_StopEnv:
				go a.dispatcher.HandleStop(context.Background(), p.StopEnv)
			case *agentpb.ControlMessage_StartEnv:
				go a.dispatcher.HandleStart(context.Background(), p.StartEnv)
			case *agentpb.ControlMessage_DeleteEnv:
				go a.dispatcher.HandleDelete(context.Background(), p.DeleteEnv)
			}
		}
	}
}()
```

NOTE: dispatcher의 sender도 stream이어야 함. 따라서 `envlifecycle.NewDispatcher`에 `stream`을 sender로 넘김. agent가 reconnect할 때마다 dispatcher의 sender를 갱신해야 함.

이를 위해 `flexctlagent`에서 dispatcher를 별도로 만들지 말고, runOnce 안에서 매 연결마다 만든다. 그렇게 하면 sender가 자연스럽게 현재 stream과 연동됨. 위 코드 수정:

```go
// runOnce 안에서 ack 수신 goroutine 시작 전에:
if a.cfg.DockerClient != nil && a.cfg.GPUAllocator != nil {
	a.dispatcher = envlifecycle.NewDispatcher(a.cfg.DockerClient, a.cfg.GPUAllocator, stream)
	envIDs, _ := a.dispatcher.BuildEnvStateSnapshot(ctx)
	_ = stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_EnvSnapshot{
			EnvSnapshot: &agentpb.EnvStateSnapshot{RunningEnvIds: envIDs},
		},
	})
}
```

그리고 `Config`에 두 필드 추가:
```go
type Config struct {
	// ... 기존 필드 ...
	DockerClient envdocker.DockerClient
	GPUAllocator *envlifecycle.GPUAllocator
}
```

#### main.go 와이어링 교체

- [ ] **Step 3: main.go 수정 — noop을 진짜 EnvsDispatcher로 교체**

`cmd/control-plane/main.go`에서 `noopEnvDispatcher` 관련 코드 제거 후, agentstream Server 생성 + EnvsDispatcher 생성을 다음 순서로:

```go
// 기존 envsSvc, tplSvc 생성 다음, 기존 grpc.NewServer() 생성 직전:
agentSrv := agentstream.NewServer(nodesSvc, envsSvc, usersSvc, sshkeysSvc, hsClient)
envDispatcher := agentstream.NewEnvsDispatcher(agentSrv, hsURL, "flex/sidecar:dev")

envsH := envs.NewHandlers(envsSvc, envDispatcher)

// 기존 grpc 서버 등록 부분에서 NewServer를 agentSrv로 교체:
agentpb.RegisterAgentServer(grpcSrv, agentSrv)
```

`sshkeysSvc`, `usersSvc` 변수는 이미 있으면 재사용. 없으면 추가:
```go
usersSvc := users.NewService(pool)
sshkeysSvc := sshkeys.NewService(pool)
```

- [ ] **Step 4: agentstream 기존 NewServer 호출자도 새 시그니처로 갱신**

기존 `agentstream.NewServer(nodesSvc)` 호출이 있던 곳:
- `cmd/control-plane/main.go` — 위에서 갱신
- `internal/agentstream/server_test.go` — 모든 호출을 `NewServer(svc, mockEnvs, mockUsers, mockKeys, mockHS)`로 갱신. 단 envs/users/keys/hs는 진짜 *Service나 새 mock으로 주입. 기존 테스트는 env 기능 안 쓰므로 nil을 넘겨도 무방하지만, runtime nil-deref를 피하려면 빈 *Service 인스턴스(예: `envs.NewService(pool)`)를 넘기는 게 안전.

테스트 헬퍼 startServer 갱신:
```go
func startServer(t *testing.T) (agentpb.AgentClient, *nodes.Service, func()) {
	t.Helper()
	pool := newTestPool(t)
	nodesSvc := nodes.NewService(pool)
	envsSvc := envs.NewService(pool)
	usersSvc := users.NewService(pool)
	keysSvc := sshkeys.NewService(pool)
	// hsClient는 기존 테스트가 env 명령 안 쓰므로 nil 무방. 단 Stream 메서드가 hs를 직접 안 쓰면 OK.
	srv := agentstream.NewServer(nodesSvc, envsSvc, usersSvc, keysSvc, nil)
	// ... 나머지 그대로 ...
}
```

newTestPool도 envs/sshkeys 테이블 모두 생성하도록 SQL 확장.

- [ ] **Step 5: 통과 확인**

```bash
go build ./...
go test ./... -race -count=1
```
Expected: 모두 PASS. 일부 테스트(특히 agentstream)는 새 시그니처에 맞춰 수정 필요할 수 있음 — 그 경우 컴파일 에러를 따라가며 수정.

- [ ] **Step 6: Commit**

```bash
git add internal/agentstream internal/flexctlagent internal/envs/service.go cmd/control-plane/main.go
git commit -m "feat: wire envs dispatcher via agentstream + flexctlagent env command routing"
```

---

### Task 13: `flexctl sidecar` 서브커맨드

**Files:**
- Create: `internal/flexctlcli/sidecar.go`
- Modify: `internal/flexctlcli/root.go` (NewSidecarCmd 등록)

이 서브커맨드는 사이드카 컨테이너 안에서만 실행됨. tailscaled 데몬을 띄우고 tailscale up 호출.

- [ ] **Step 1: 구현**

`internal/flexctlcli/sidecar.go`:
```go
package flexctlcli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func NewSidecarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sidecar",
		Short: "Run inside the sidecar container: bring up tailscaled + tailscale up",
		Hidden: true, // 사용자에게 직접 노출 X
		RunE: func(cmd *cobra.Command, _ []string) error {
			hostname := os.Getenv("FLEXCTL_HOSTNAME")
			authkey := os.Getenv("FLEXCTL_AUTHKEY")
			hsURL := os.Getenv("FLEXCTL_HEADSCALE_URL")
			tags := os.Getenv("FLEXCTL_TAGS")
			if hostname == "" || authkey == "" || hsURL == "" {
				return errors.New("FLEXCTL_HOSTNAME, FLEXCTL_AUTHKEY, FLEXCTL_HEADSCALE_URL required")
			}

			if err := os.MkdirAll("/var/lib/tailscale", 0o755); err != nil {
				return fmt.Errorf("mkdir state: %w", err)
			}
			if err := os.MkdirAll("/var/run/tailscale", 0o755); err != nil {
				return fmt.Errorf("mkdir run: %w", err)
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			daemon := exec.CommandContext(ctx, "tailscaled",
				"--state=/var/lib/tailscale/tailscaled.state",
				"--socket=/var/run/tailscale/tailscaled.sock",
				"--tun=tailscale0")
			daemon.Stdout = os.Stdout
			daemon.Stderr = os.Stderr
			if err := daemon.Start(); err != nil {
				return fmt.Errorf("start tailscaled: %w", err)
			}
			slog.Info("tailscaled started", "pid", daemon.Process.Pid)

			// Wait briefly for the daemon socket
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat("/var/run/tailscale/tailscaled.sock"); err == nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}

			upArgs := []string{
				"--login-server=" + hsURL,
				"--authkey=" + authkey,
				"--hostname=" + hostname,
				"--accept-dns=false",
			}
			if t := strings.TrimSpace(tags); t != "" {
				upArgs = append(upArgs, "--advertise-tags="+t)
			}
			up := exec.CommandContext(ctx, "tailscale", append([]string{"up"}, upArgs...)...)
			up.Stdout = os.Stdout
			up.Stderr = os.Stderr
			if err := up.Run(); err != nil {
				_ = daemon.Process.Signal(syscall.SIGTERM)
				return fmt.Errorf("tailscale up: %w", err)
			}
			slog.Info("tailscale up succeeded", "hostname", hostname)

			// Wait for SIGTERM, then logout + stop daemon.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			<-sigCh
			slog.Info("sidecar shutting down")
			_ = exec.Command("tailscale", "logout").Run()
			_ = daemon.Process.Signal(syscall.SIGTERM)
			_ = daemon.Wait()
			return nil
		},
	}
}
```

- [ ] **Step 2: root.go 등록**

`internal/flexctlcli/root.go`의 `NewRootCmd`에 추가:
```go
	root.AddCommand(NewSidecarCmd())
```

- [ ] **Step 3: 빌드 확인**

```bash
make build-flexctl
./bin/flexctl sidecar --help
```
Expected: `sidecar` 명령 출력 (Hidden이지만 --help는 동작).

`./bin/flexctl --help`에서는 sidecar가 안 보여야 함 (Hidden:true).

- [ ] **Step 4: Commit**

```bash
git add internal/flexctlcli/sidecar.go internal/flexctlcli/root.go
git commit -m "feat(flexctl): sidecar subcommand for in-container tailscaled bringup"
```

---

### Task 14: 사이드카/dev Dockerfile + Makefile 타겟

**Files:**
- Create: `images/sidecar/Dockerfile`
- Create: `images/dev-cuda-base/Dockerfile`
- Create: `images/dev-cuda-base/entrypoint.sh`
- Modify: `Makefile` (sidecar-image / dev-image 타겟)

- [ ] **Step 1: 사이드카 Dockerfile**

`images/sidecar/Dockerfile`:
```dockerfile
FROM alpine:3.20
RUN apk add --no-cache iptables ip6tables ca-certificates wget tar
ARG TS_VERSION=1.76.6
RUN wget -q https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_amd64.tgz \
 && tar xzf tailscale_${TS_VERSION}_amd64.tgz \
 && mv tailscale_${TS_VERSION}_amd64/tailscaled /usr/local/bin/ \
 && mv tailscale_${TS_VERSION}_amd64/tailscale  /usr/local/bin/ \
 && rm -rf tailscale_${TS_VERSION}_amd64*
COPY bin/flexctl /usr/local/bin/flexctl
RUN chmod +x /usr/local/bin/flexctl
ENTRYPOINT ["/usr/local/bin/flexctl", "sidecar"]
```

- [ ] **Step 2: dev base Dockerfile + entrypoint**

`images/dev-cuda-base/Dockerfile`:
```dockerfile
FROM nvidia/cuda:12.4.1-base-ubuntu22.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    openssh-server sudo python3 python3-pip vim curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash dev \
 && mkdir -p /home/dev/.ssh && chown -R dev:dev /home/dev/.ssh && chmod 700 /home/dev/.ssh \
 && mkdir /run/sshd
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

`images/dev-cuda-base/entrypoint.sh`:
```bash
#!/bin/sh
set -e
if [ -n "$FLEXCTL_AUTHORIZED_KEYS" ]; then
  printf '%s\n' "$FLEXCTL_AUTHORIZED_KEYS" > /home/dev/.ssh/authorized_keys
  chmod 600 /home/dev/.ssh/authorized_keys
  chown dev:dev /home/dev/.ssh/authorized_keys
fi
exec /usr/sbin/sshd -D -e
```

- [ ] **Step 3: Makefile 타겟**

기존 `.PHONY` 라인에 추가: `sidecar-image dev-image`

파일 끝에 타겟 추가:
```make
sidecar-image: build-flexctl
	docker build -t flex/sidecar:dev -f images/sidecar/Dockerfile .

dev-image:
	docker build -t flex/dev-cuda-base:dev images/dev-cuda-base
```

NOTE: `sidecar-image`은 빌드된 `bin/flexctl`을 컨텍스트에 포함하기 위해 repo root에서 `docker build`. Dockerfile에서 `COPY bin/flexctl ...` 라인 참조.

- [ ] **Step 4: 빌드 검증**

```bash
make build-flexctl
make sidecar-image
docker images flex/sidecar
# Expected: flex/sidecar dev <id> <created> <size>

make dev-image
docker images flex/dev-cuda-base
# Expected: 마찬가지

# 사이드카 ENTRYPOINT가 flexctl sidecar 호출하는지 sanity
docker run --rm flex/sidecar:dev --help 2>&1 | head -5
# Expected: "Run inside the sidecar container..." 등의 cobra 출력
```

GPU 없는 dev 머신이면 dev-image도 빌드는 됨 (런타임 호출만 GPU 필요).

- [ ] **Step 5: Commit**

```bash
git add images/ Makefile
git commit -m "feat: sidecar + dev-cuda-base Dockerfiles and Makefile targets"
```

---

### Task 15: E2E 통합 테스트

**Files:**
- Create: `internal/envs/e2e_test.go`

이 테스트는 진짜 Postgres + 진짜 Headscale + 진짜 gRPC 서버 + 진짜 flexctlagent goroutine + MockDockerClient로 e2e 플로우를 검증. HTTP `POST /v1/envs` → 컨트롤 플레인 → gRPC stream → agent dispatcher (mock docker) → EnvReady → DB status=running. SSH 실제 연결은 Plan 5에서.

- [ ] **Step 1: 작성**

`internal/envs/e2e_test.go`:
```go
package envs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/envdocker"
	"github.com/paul/flexctl/internal/envlifecycle"
	"github.com/paul/flexctl/internal/envs"
	"github.com/paul/flexctl/internal/flexctlagent"
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/nodes"
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

func TestE2E_CreateEnvReachesRunning(t *testing.T) {
	pool := newPool(t)
	usersSvc := users.NewService(pool)
	nodesSvc := nodes.NewService(pool)
	envsSvc := envs.NewService(pool)
	keysSvc := sshkeys.NewService(pool)

	// User + node 페어링
	u, err := usersSvc.Signup(context.Background(), "e@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	_, err = keysSvc.Add(context.Background(), u.ID, "test",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBM5dWmqyhEfP9C1ZDjmh+e9zYx7DbT6JqnNK7NqQy11 paul@laptop")
	require.NoError(t, err)
	pairTok, _ := nodesSvc.CreatePairToken(context.Background(), u.ID)
	node, nodeToken, err := nodesSvc.PairNode(context.Background(), nodes.PairRequest{
		Token: pairTok, Name: "rtx",
	})
	require.NoError(t, err)

	// Headscale + control-plane Headscale user (테스트에서 키 발급에 필요)
	// Headscale 컨테이너는 startHeadscale로 띄움 (Plan 2 testhelpers와 동일 패턴)
	hsURL, hsAPIKey := startHeadscaleForTest(t)
	hsClient := headscale.NewClient(hsURL, hsAPIKey, 5*time.Second)
	_, err = hsClient.CreateUser(context.Background(), "paul")
	require.NoError(t, err)

	// gRPC 서버
	agentSrv := agentstream.NewServer(nodesSvc, envsSvc, usersSvc, keysSvc, hsClient)
	envDispatcher := agentstream.NewEnvsDispatcher(agentSrv, hsURL, "flex/sidecar:dev")
	envsH := envs.NewHandlers(envsSvc, envDispatcher)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcSrv, agentSrv)
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	// HTTP 서버
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		envsH.Mount(r)
	})
	httpSrv := httptest.NewServer(r)
	defer httpSrv.Close()

	// Mock Docker + dispatcher가 들어간 agent 띄우기
	mockDocker := envdocker.NewMockDockerClient()
	alloc := envlifecycle.NewGPUAllocator([]int{0, 1})
	// 사이드카 컨테이너의 env_id는 컨트롤 플레인이 동적 생성하므로 미리 ExecOutput
	// 키를 못 박아둠. SetExecOutputForRole 헬퍼(mock.go에 추가 필요)로 사이드카가 만들어진
	// 후 폴링 goroutine에서 응답을 set한다 (아래 watcher goroutine 참조).

	agentCfg := flexctlagent.Config{
		GRPCAddress:       lis.Addr().String(),
		NodeToken:         nodeToken,
		AgentVersion:      "0.1.0-test",
		HeartbeatInterval: 100 * time.Millisecond,
		DockerClient:      mockDocker,
		GPUAllocator:      alloc,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := flexctlagent.New(agentCfg)
	go func() { _ = agent.Run(ctx) }()

	// agent 연결 대기
	time.Sleep(300 * time.Millisecond)

	// 환경 생성 요청 — env_id는 컨트롤 플레인이 생성하므로 MockDocker의 ExecOutput 키를
	// 미리 못 세팅. 대신 prepareCreate에서 후처리로 보강.
	// 그냥 처음 env가 만들어진 시점에 Exec 응답이 빈 string → health timeout 발생 우려.
	// 단순화: dispatcher가 어떤 ID로 만들든 Exec이 "100." 포함하도록 fallback 설정.
	mockDocker.ExecOutput[""] = "tailscale 100.64.0.5\n" // 기본값
	// MockDockerClient의 Exec 메서드는 키 없으면 빈 string 반환 — 이를 보완하기 위해
	// 테스트 helper로 patcher를 만들거나 mockDocker.ExecOutput에 모든 키에 대해 동일 응답을
	// 반환하는 fallback 로직이 필요.
	// 본 테스트에선 dispatcher 호출 후 ExecOutput을 set하기 위해 별도 goroutine으로 watch.
	go func() {
		for {
			time.Sleep(50 * time.Millisecond)
			mockDocker.Mu.Lock()
			for id := range mockDocker.Containers {
				if mockDocker.Containers[id].Labels[envdocker.LabelRole] == "sidecar" {
					mockDocker.ExecOutput[id] = "tailscale 100.64.0.5\n"
				}
			}
			mockDocker.Mu.Unlock()
		}
	}()

	tok, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})
	body, _ := json.Marshal(map[string]any{
		"node_id":     node.ID.String(),
		"template_id": "cuda-base",
		"name":        "e2e-test",
		"gpu_request": 1,
	})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/envs", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// DB 폴링 — status=running이 될 때까지
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := envsSvc.ListByOwner(context.Background(), u.ID)
		if len(got) == 1 && got[0].Status == "running" {
			require.NotEmpty(t, got[0].SidecarContainerID)
			require.NotEmpty(t, got[0].DevContainerID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, _ := envsSvc.ListByOwner(context.Background(), u.ID)
	if len(got) > 0 {
		t.Fatalf("env never reached running; status=%s message=%s", got[0].Status, got[0].StatusMessage)
	}
	t.Fatal("env not created")
}

// startHeadscaleForTest는 Plan 2의 internal/headscale/testhelpers_test.go의 startHeadscale
// 함수 본문을 그대로 옮겨온 것. (testcontainers headscale:0.23.0 + config/headscale-test.yaml
// mount + `headscale users create control-plane` + apikey 발급 + (baseURL, apiKey) 반환)
// _test.go 간 import 불가라 복사. 또는 internal/testutil 패키지로 추출 (별도 plan).
//
// 구현 시: internal/headscale/testhelpers_test.go 파일을 열어 startHeadscale, readAll,
// extractAPIKey, longestTokenRun 4개 함수를 그대로 복사하고 startHeadscale → startHeadscaleForTest로
// 이름만 바꾼다.
```

NOTE: MockDockerClient에 `Mu` 필드를 export하거나 ExecOutput에 동시 접근 방법이 필요. MockDockerClient의 `mu` 필드는 unexported. 두 가지 선택:
- (a) MockDockerClient에 `SetExecOutput(id, stdout string)` 메서드 추가해 race-safe하게 갱신
- (b) 테스트에서 goroutine watcher 대신, dispatcher의 sidecar Exec 응답을 inject할 다른 방법 (예: dispatcher의 health check polls interval을 짧게 두고 Exec에 fallback 추가)

가장 깔끔한 답: MockDockerClient에 `SetExecOutput`/`SetExecOutputForLabel` 메서드 추가. 또는 ExecOutput map에 빈 키("")가 있으면 fallback으로 그 값을 반환하도록 `Exec` 메서드 수정.

implementer는 두 옵션 중 골라서 진행. 본 plan에선 (a) 권장:
```go
// internal/envdocker/mock.go에 추가
func (m *MockDockerClient) SetExecOutputForRole(role, stdout string) {
	m.mu.Lock(); defer m.mu.Unlock()
	for id, c := range m.Containers {
		if c.Labels[LabelRole] == role { m.ExecOutput[id] = stdout }
	}
}
```

그러고 e2e 테스트에서 watcher goroutine 대신:
```go
go func() {
	for ctx.Err() == nil {
		mockDocker.SetExecOutputForRole("sidecar", "tailscale 100.64.0.5\n")
		time.Sleep(50 * time.Millisecond)
	}
}()
```

- [ ] **Step 2: 헬퍼 옮겨오기**

`internal/headscale/testhelpers_test.go`의 `startHeadscale` 함수 본문을 `internal/envs/e2e_test.go`에 복사 (이름은 `startHeadscaleForTest` 또는 동일). 두 package_test 간 코드 중복이지만 _test 파일 import 제약 때문에 불가피.

또는 별도 `internal/testutil` 패키지로 추출 — Plan 4 범위는 단순 복사.

- [ ] **Step 3: 실행 확인**

```bash
go test ./internal/envs/ -race -count=1 -run TestE2E -v -timeout 120s
```

Expected: 1 PASS (Headscale 컨테이너 + Postgres 컨테이너 시작에 30~60초 소요).

- [ ] **Step 4: 전체 suite 확인**

```bash
go test ./... -race -count=1 -timeout 180s
```
Expected: 모두 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/envdocker/mock.go internal/envs/e2e_test.go
git commit -m "test(envs): e2e integration via http → grpc → mock docker → status=running"
```

---

### Task 16: README 갱신

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 환경 변수 표에 행 추가 (필요시)**

기존 환경 변수는 FLEX_GRPC_ADDR까지. 새 변수는 없음 — 모든 env 동작은 기존 환경에서 작동.

- [ ] **Step 2: 엔드포인트 표에 행 추가**

`README.md`의 "현재 노출된 엔드포인트" 표에 다음 행 추가 (nodes 다음에):
```markdown
| GET | `/v1/image-templates` | session | 사용 가능한 dev 이미지 템플릿 목록 |
| GET | `/v1/envs` | session | 내 환경 목록 |
| POST | `/v1/envs` | session | 환경 생성 (사이드카+dev 컨테이너 묶음) |
| GET | `/v1/envs/{id}` | session | 환경 단건 조회 |
| POST | `/v1/envs/{id}/stop` | session | 환경 중지 |
| POST | `/v1/envs/{id}/start` | session | 환경 시작 |
| DELETE | `/v1/envs/{id}` | session | 환경 삭제 (볼륨 포함) |
```

- [ ] **Step 3: "환경 생성" 섹션 추가**

파일 끝에 추가:
```markdown
## 환경 생성 + 사이드카 빌드

### 사이드카 + dev 이미지 빌드 (운영자 1회)

GPU 서버에 다음 두 이미지가 로컬에 있어야 함 (Plan 4 MVP — registry push 없음):

    make build-flexctl       # bin/flexctl 빌드 (사이드카 Dockerfile이 COPY)
    make sidecar-image       # → flex/sidecar:dev
    make dev-image           # → flex/dev-cuda-base:dev

이 두 이미지는 agent가 동작하는 노드에 존재해야 함. dev 머신에서 빌드 후 GPU 서버로 push하거나, GPU 서버에서 직접 빌드.

### 환경 생성 e2e

페어링된 노드가 있다고 가정:

    curl -fsS -X POST http://localhost:8080/v1/envs \
      -H 'content-type: application/json' \
      -b /tmp/c.txt \
      -d '{"node_id":"<uuid>","template_id":"cuda-base","name":"vllm","gpu_request":1}'
    # → 202 Accepted + env JSON (status: "creating")

    # 잠시 후 status 확인:
    curl -s -b /tmp/c.txt http://localhost:8080/v1/envs | jq
    # → [{ "status": "running", "hostname": "paul-vllm", ... }]

    # 노드에서 컨테이너 두 개 확인:
    docker ps --filter label=flexctl.env_id=<env-id>
    # → flex-net-<id> (사이드카) + flex-env-<id> (dev)

SSH 접속은 **Plan 5 (flexctl client)**가 완료되면 가능.

### 중지 / 시작 / 삭제

    curl -X POST -b /tmp/c.txt http://localhost:8080/v1/envs/<id>/stop
    curl -X POST -b /tmp/c.txt http://localhost:8080/v1/envs/<id>/start
    curl -X DELETE -b /tmp/c.txt http://localhost:8080/v1/envs/<id>

Stop은 컨테이너만 정지 (볼륨 유지). Delete는 볼륨까지 제거 — 데이터 영구 손실.
```

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: README envs endpoints + sidecar/dev image build instructions"
```

---

## End-to-End 검증 체크리스트

Plan 4 구현 완료 후 다음을 모두 통과해야 합니다.

- [ ] `make test`: 기존 78 + 신규 약 30 = 108개 이상 테스트 PASS
- [ ] `go vet ./...`: 경고 없음
- [ ] `make build` + `make build-flexctl`: 둘 다 성공
- [ ] `make sidecar-image` + `make dev-image`: 두 Docker 이미지 빌드 성공
- [ ] 통합 e2e 테스트 (`TestE2E_CreateEnvReachesRunning`): 진짜 Postgres + 진짜 Headscale + mock Docker로 status=running 도달
- [ ] (선택, GPU 머신) 실제 시나리오:
  - 컨트롤 플레인 + Headscale + flexctl agent 모두 가동
  - `POST /v1/envs` → 5초 내 `docker ps`에 두 컨테이너 가시화, `headscale nodes list`에 새 노드 등장
  - 사이드카 컨테이너에서 `tailscale status` 호출 시 `100.x.x.x` IP 보고
  - DB의 envs.status = `running`, gpu_indices 비어있지 않음 (gpu_request > 0인 경우)
- [ ] Stop → containers exited, gpu_indices 비워짐
- [ ] Start → 사이드카 재생성(새 preauth_key) + dev 재시작 → status=running
- [ ] Delete → 모든 컨테이너 + 볼륨 제거 + DB row 삭제
- [ ] agent process restart 후 EnvStateSnapshot 전송 → 컨트롤 플레인이 lost env들을 status=error로 마킹

이 체크리스트가 통과되면 **Plan 5 (flexctl client + tsnet + ProxyCommand)**로 진행해서 실제 SSH 접속까지 완성합니다.
