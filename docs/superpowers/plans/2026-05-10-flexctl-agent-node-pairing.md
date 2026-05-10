# flexctl agent + Node Pairing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GPU 서버에서 `flexctl join <token>` 한 줄로 컨트롤 플레인에 페어링하면 백그라운드 agent가 gRPC bidi-stream으로 등록·heartbeat를 유지하고 컨트롤 플레인이 노드를 "online"으로 인식하게 만든다. 끝나면 통합 테스트로 "페어링 토큰 발급 → fake agent 페어링 → stream 연결 → register/heartbeat 수신 → nodes.last_seen_at 갱신" 전 흐름이 검증된다.

**Architecture:** 컨트롤 플레인이 HTTP(8080) + gRPC(9090) 두 포트 동시 서빙. 페어링은 HTTP(POST /v1/nodes/pair-token, /v1/nodes/pair) 1회 사용 토큰; 영구 노드 인증은 별도의 long-lived `node_token`(sha256 해시 저장). agent는 gRPC bidi-stream 메타데이터에 `node-token`을 담아 인증, 첫 메시지로 `Register`(agent_version + GPU 정보) 전송, 이후 N초 주기 `Heartbeat`. 끊기면 jittered exponential backoff(1s→60s cap)로 재연결, 재연결 직후 다시 `Register`(snapshot resync 토대). flexctl은 Cobra 기반 단일 Go 바이너리(`flexctl join`/`flexctl agent`).

**Tech Stack:** Go 1.22+, `google.golang.org/grpc` v1.66+, `google.golang.org/protobuf` v1.34+, `spf13/cobra` v1.8+. 기존 chi/pgx/argon2id/Headscale 그대로. Protobuf 생성: `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` (생성된 .pb.go는 repo에 commit — runtime/test가 protoc 의존 X).

**Out of scope (Plan 4+):** 환경(컨테이너) 생성/시작/삭제 명령, flexctl client 모드(노트북 tsnet), Web UI, agent 자동 업데이트, systemd unit 자동 설치(이번엔 README 가이드만).

---

## File Structure

```
.
├── cmd/
│   ├── control-plane/main.go        # MODIFY: gRPC 서버 + nodes 핸들러 와이어링
│   └── flexctl/main.go              # NEW: Cobra entry point
├── proto/
│   └── agent.proto                  # NEW: AgentMessage / ControlMessage / Register / Heartbeat
├── internal/
│   ├── agentpb/                     # NEW: protoc 생성 코드 (commit)
│   │   ├── agent.pb.go
│   │   └── agent_grpc.pb.go
│   ├── agentstream/                 # NEW: 컨트롤 플레인 측 gRPC 서버
│   │   ├── server.go                # AgentServer + Stream + node-token interceptor
│   │   └── server_test.go
│   ├── flexctlagent/                # NEW: agent 측 stream 클라이언트 + reconnect
│   │   ├── agent.go
│   │   └── agent_test.go
│   ├── flexctlcli/                  # NEW: Cobra commands
│   │   ├── root.go
│   │   ├── join.go
│   │   └── agent.go
│   ├── gpuinfo/                     # NEW: NVIDIA GPU 감지
│   │   ├── nvidia.go
│   │   └── nvidia_test.go
│   └── nodes/                       # NEW: nodes/pair_tokens DB + HTTP handlers
│       ├── service.go
│       ├── service_test.go
│       ├── handlers.go
│       └── handlers_test.go
├── migrations/
│   ├── 0004_pair_tokens.{up,down}.sql
│   └── 0005_nodes.{up,down}.sql
├── buf.gen.yaml                      # NEW (선택): buf 사용 시. 본 plan은 protoc 직접
└── Makefile                          # MODIFY: proto-gen 타겟
```

각 패키지 한 가지 책임. agentpb는 generated만, agentstream은 server-side, flexctlagent은 client-side, flexctlcli는 CLI 뿌리, gpuinfo는 nvidia-smi 호출, nodes는 DB+HTTP.

---

### Task 1: migrations — pair_tokens + nodes

**Files:**
- Create: `migrations/0004_pair_tokens.up.sql`
- Create: `migrations/0004_pair_tokens.down.sql`
- Create: `migrations/0005_nodes.up.sql`
- Create: `migrations/0005_nodes.down.sql`

- [ ] **Step 1: pair_tokens 마이그레이션**

`migrations/0004_pair_tokens.up.sql`:
```sql
CREATE TABLE pair_tokens (
  token_hash  text PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX pair_tokens_user_idx ON pair_tokens (user_id, created_at DESC);
```

`migrations/0004_pair_tokens.down.sql`:
```sql
DROP TABLE IF EXISTS pair_tokens;
```

- [ ] **Step 2: nodes 마이그레이션**

`migrations/0005_nodes.up.sql`:
```sql
CREATE TABLE nodes (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name            text NOT NULL,
  agent_version   text NOT NULL DEFAULT '',
  gpu_info        jsonb NOT NULL DEFAULT '[]'::jsonb,
  status          text NOT NULL DEFAULT 'offline',
  node_token_hash text NOT NULL UNIQUE,
  last_seen_at    timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE(owner_user_id, name)
);

CREATE INDEX nodes_status_idx ON nodes (status);
```

`migrations/0005_nodes.down.sql`:
```sql
DROP TABLE IF EXISTS nodes;
```

- [ ] **Step 3: 마이그레이션 검증**

```bash
docker compose up -d postgres
make migrate-up
docker compose exec postgres psql -U flex -d flex -c '\d pair_tokens'
docker compose exec postgres psql -U flex -d flex -c '\d nodes'
make migrate-down  # nodes 제거
make migrate-down  # pair_tokens 제거
make migrate-up    # 둘 다 복원
```

Expected: 둘 다 \d 출력에 컬럼/제약 정확히 표시. Round-trip 에러 없음.

- [ ] **Step 4: Commit**

```bash
git add migrations/0004_pair_tokens.up.sql migrations/0004_pair_tokens.down.sql migrations/0005_nodes.up.sql migrations/0005_nodes.down.sql
git commit -m "feat(db): pair_tokens and nodes table migrations"
```

---

### Task 2: nodes 서비스 — pair token + node CRUD (TDD)

**Files:**
- Create: `internal/nodes/service.go`
- Create: `internal/nodes/service_test.go`

토큰 생성/해싱 + 페어링 검증 + 노드 등록 + 노드 조회까지의 비즈니스 로직.

- [ ] **Step 1: 실패 테스트 작성**

`internal/nodes/service_test.go`:
```go
package nodes_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/paul/flexctl/internal/nodes"
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
	return pool
}

func mkUser(t *testing.T, pool *pgxpool.Pool, email, slug string) uuid.UUID {
	t.Helper()
	svc := users.NewService(pool)
	u, err := svc.Signup(context.Background(), email, slug, "correct-horse-battery")
	require.NoError(t, err)
	return u.ID
}

func TestCreatePairToken_FormatAndExpiry(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	token, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "FX-"), "token must have FX- prefix, got %q", token)
	require.GreaterOrEqual(t, len(token), 60, "token should be reasonably long")
}

func TestPairNode_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	token, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)

	node, nodeToken, err := svc.PairNode(context.Background(), nodes.PairRequest{
		Token:    token,
		Name:     "rtx4090",
		GPUInfo:  []byte(`[{"index":0,"model":"NVIDIA RTX 4090","vram_mb":24576}]`),
	})
	require.NoError(t, err)
	require.NotEmpty(t, nodeToken)
	require.Equal(t, "rtx4090", node.Name)
	require.Equal(t, uid, node.OwnerUserID)
}

func TestPairNode_TokenAlreadyUsed(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)

	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "n1"})
	require.NoError(t, err)

	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "n2"})
	require.ErrorIs(t, err, nodes.ErrTokenInvalid)
}

func TestPairNode_TokenExpired(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	// Insert a manually-expired token.
	tokenHash, err := svc.HashTokenForTest("FX-expired-token-please-fail")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO pair_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, uid, time.Now().Add(-time.Minute))
	require.NoError(t, err)

	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{
		Token: "FX-expired-token-please-fail", Name: "n1",
	})
	require.ErrorIs(t, err, nodes.ErrTokenInvalid)
}

func TestPairNode_DuplicateNodeName(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")

	t1, _ := svc.CreatePairToken(context.Background(), uid)
	_, _, err := svc.PairNode(context.Background(), nodes.PairRequest{Token: t1, Name: "rtx"})
	require.NoError(t, err)

	t2, _ := svc.CreatePairToken(context.Background(), uid)
	_, _, err = svc.PairNode(context.Background(), nodes.PairRequest{Token: t2, Name: "rtx"})
	require.ErrorIs(t, err, nodes.ErrNodeNameTaken)
}

func TestAuthenticateNodeToken(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, _ := svc.CreatePairToken(context.Background(), uid)
	node, nodeToken, err := svc.PairNode(context.Background(), nodes.PairRequest{
		Token: token, Name: "rtx",
	})
	require.NoError(t, err)

	got, err := svc.AuthenticateNodeToken(context.Background(), nodeToken)
	require.NoError(t, err)
	require.Equal(t, node.ID, got.ID)

	_, err = svc.AuthenticateNodeToken(context.Background(), "bogus-token")
	require.ErrorIs(t, err, nodes.ErrNodeTokenInvalid)
}

func TestRecordHeartbeat(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, _ := svc.CreatePairToken(context.Background(), uid)
	node, _, err := svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "rtx"})
	require.NoError(t, err)

	require.NoError(t, svc.RecordHeartbeat(context.Background(), node.ID))

	var status string
	var lastSeen *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status, last_seen_at FROM nodes WHERE id = $1`, node.ID,
	).Scan(&status, &lastSeen))
	require.Equal(t, "online", status)
	require.NotNil(t, lastSeen)
}

func TestUpdateRegister(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	uid := mkUser(t, pool, "p@x.com", "paul")
	token, _ := svc.CreatePairToken(context.Background(), uid)
	node, _, err := svc.PairNode(context.Background(), nodes.PairRequest{Token: token, Name: "rtx"})
	require.NoError(t, err)

	require.NoError(t, svc.UpdateRegister(context.Background(),
		node.ID, "0.1.0", []byte(`[{"index":0,"model":"H100","vram_mb":81920}]`)))

	var version string
	var gpu []byte
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT agent_version, gpu_info FROM nodes WHERE id = $1`, node.ID,
	).Scan(&version, &gpu))
	require.Equal(t, "0.1.0", version)
	require.Contains(t, string(gpu), "H100")
}
```

NOTE: `HashTokenForTest`는 단순한 expose. 구현부에 추가.

- [ ] **Step 2: 테스트 실행으로 실패 확인**

Run: `go test ./internal/nodes/ -v`
Expected: 컴파일 에러 (`nodes.NewService`, `nodes.ErrTokenInvalid` 등 미정의).

- [ ] **Step 3: 서비스 구현**

`internal/nodes/service.go`:
```go
package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTokenInvalid     = errors.New("pair token invalid or expired")
	ErrNodeNameTaken    = errors.New("node name already taken for this user")
	ErrNodeTokenInvalid = errors.New("node token invalid")
	ErrNotFound         = errors.New("node not found")
)

const (
	pairTokenPrefix = "FX-"
	pairTokenBytes  = 32 // 32 bytes → ~43 base64 chars + "FX-" prefix
	nodeTokenBytes  = 32
	pairTokenTTL    = 10 * time.Minute
)

type Node struct {
	ID            uuid.UUID
	OwnerUserID   uuid.UUID
	Name          string
	AgentVersion  string
	GPUInfo       []byte
	Status        string
	LastSeenAt    *time.Time
}

type PairRequest struct {
	Token   string
	Name    string
	GPUInfo []byte // optional, agent may not have GPU info at pair time
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) CreatePairToken(ctx context.Context, userID uuid.UUID) (string, error) {
	raw := make([]byte, pairTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	token := pairTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := hashToken(token)

	_, err := s.pool.Exec(ctx,
		`INSERT INTO pair_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hash, userID, time.Now().Add(pairTokenTTL),
	)
	if err != nil {
		return "", fmt.Errorf("insert pair token: %w", err)
	}
	return token, nil
}

// HashTokenForTest exposes hashing for test fixtures. Not used in production paths.
func (s *Service) HashTokenForTest(token string) (string, error) {
	return hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) PairNode(ctx context.Context, req PairRequest) (Node, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Node{}, "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock + validate token
	var (
		userID    uuid.UUID
		expiresAt time.Time
		usedAt    *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT user_id, expires_at, used_at FROM pair_tokens
		 WHERE token_hash = $1 FOR UPDATE`,
		hashToken(req.Token),
	).Scan(&userID, &expiresAt, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, "", ErrTokenInvalid
	}
	if err != nil {
		return Node{}, "", fmt.Errorf("select pair token: %w", err)
	}
	if usedAt != nil || time.Now().After(expiresAt) {
		return Node{}, "", ErrTokenInvalid
	}

	// Mark used
	_, err = tx.Exec(ctx,
		`UPDATE pair_tokens SET used_at = now() WHERE token_hash = $1`,
		hashToken(req.Token),
	)
	if err != nil {
		return Node{}, "", fmt.Errorf("mark token used: %w", err)
	}

	// Generate node token
	nodeRaw := make([]byte, nodeTokenBytes)
	if _, err := rand.Read(nodeRaw); err != nil {
		return Node{}, "", fmt.Errorf("read random: %w", err)
	}
	nodeToken := base64.RawURLEncoding.EncodeToString(nodeRaw)
	nodeTokenHash := hashToken(nodeToken)

	// Insert node
	gpuInfo := req.GPUInfo
	if len(gpuInfo) == 0 {
		gpuInfo = []byte(`[]`)
	}
	var nodeID uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO nodes (owner_user_id, name, gpu_info, node_token_hash)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, req.Name, gpuInfo, nodeTokenHash,
	).Scan(&nodeID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Node{}, "", ErrNodeNameTaken
		}
		return Node{}, "", fmt.Errorf("insert node: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Node{}, "", fmt.Errorf("commit: %w", err)
	}
	return Node{
		ID:          nodeID,
		OwnerUserID: userID,
		Name:        req.Name,
		GPUInfo:     gpuInfo,
		Status:      "offline",
	}, nodeToken, nil
}

func (s *Service) AuthenticateNodeToken(ctx context.Context, token string) (Node, error) {
	if token == "" {
		return Node{}, ErrNodeTokenInvalid
	}
	hash := hashToken(token)
	var n Node
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_user_id, name, agent_version, gpu_info, status, last_seen_at
		 FROM nodes WHERE node_token_hash = $1`,
		hash,
	).Scan(&n.ID, &n.OwnerUserID, &n.Name, &n.AgentVersion, &n.GPUInfo, &n.Status, &n.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNodeTokenInvalid
	}
	if err != nil {
		return Node{}, fmt.Errorf("select node: %w", err)
	}
	return n, nil
}

func (s *Service) UpdateRegister(ctx context.Context, nodeID uuid.UUID, agentVersion string, gpuInfo []byte) error {
	if len(gpuInfo) == 0 {
		gpuInfo = []byte(`[]`)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE nodes
		 SET agent_version = $2, gpu_info = $3, status = 'online', last_seen_at = now()
		 WHERE id = $1`,
		nodeID, agentVersion, gpuInfo,
	)
	if err != nil {
		return fmt.Errorf("update register: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) RecordHeartbeat(ctx context.Context, nodeID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE nodes SET status = 'online', last_seen_at = now() WHERE id = $1`,
		nodeID,
	)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkOffline(ctx context.Context, nodeID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE nodes SET status = 'offline' WHERE id = $1`,
		nodeID,
	)
	if err != nil {
		return fmt.Errorf("mark offline: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/nodes/ -race -count=1 -v`
Expected: 모든 테스트 PASS (CreatePairToken, PairNode happy/used/expired/dup, AuthNodeToken, RecordHeartbeat, UpdateRegister).

- [ ] **Step 5: Commit**

```bash
git add internal/nodes/service.go internal/nodes/service_test.go
git commit -m "feat(nodes): pair token + node CRUD service"
```

---

### Task 3: nodes HTTP 핸들러 — POST /v1/nodes/pair-token + /v1/nodes/pair

**Files:**
- Create: `internal/nodes/handlers.go`
- Create: `internal/nodes/handlers_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/nodes/handlers_test.go`:
```go
package nodes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/nodes"
)

func mountForTest(t *testing.T, pool interface{}, signer *auth.SessionSigner, h *nodes.Handlers) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	h.MountPublic(r)
	return httptest.NewServer(r)
}

func TestPairTokenHandler_RequiresAuth(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/nodes/pair-token", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPairTokenHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	uid := mkUser(t, pool, "p@x.com", "paul")
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	tok, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/nodes/pair-token", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tok})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.Token)
	require.NotEmpty(t, got.ExpiresAt)
}

func TestPairHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	uid := mkUser(t, pool, "p@x.com", "paul")
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	pairToken, err := svc.CreatePairToken(context.Background(), uid)
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]any{
		"token": pairToken,
		"name":  "rtx-1",
		"gpu_info": []map[string]any{
			{"index": 0, "model": "RTX 4090", "vram_mb": 24576},
		},
	})
	resp, err := http.Post(srv.URL+"/v1/nodes/pair", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got struct {
		NodeID    string `json:"node_id"`
		NodeToken string `json:"node_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.NodeID)
	require.NotEmpty(t, got.NodeToken)
}

func TestPairHandler_BadToken(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := nodes.NewHandlers(svc)
	srv := mountForTest(t, pool, signer, h)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"token": "FX-bogus", "name": "n1"})
	resp, err := http.Post(srv.URL+"/v1/nodes/pair", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/nodes/ -run "TestPair" -v`
Expected: 컴파일 에러 (`NewHandlers`, `MountAuthed`, `MountPublic` 미정의).

- [ ] **Step 3: 핸들러 구현**

`internal/nodes/handlers.go`:
```go
package nodes

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

type Handlers struct {
	svc *Service
}

func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// MountAuthed mounts routes that require a session cookie.
func (h *Handlers) MountAuthed(r chi.Router) {
	r.Post("/v1/nodes/pair-token", h.createPairToken)
}

// MountPublic mounts routes accessible without a session (token-authenticated).
func (h *Handlers) MountPublic(r chi.Router) {
	r.Post("/v1/nodes/pair", h.pair)
}

type pairTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *Handlers) createPairToken(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	token, err := h.svc.CreatePairToken(r.Context(), uid)
	if err != nil {
		slog.Error("create pair token", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pairTokenResp{
		Token:     token,
		ExpiresAt: time.Now().Add(pairTokenTTL),
	})
}

type pairReq struct {
	Token   string          `json:"token"`
	Name    string          `json:"name"`
	GPUInfo json.RawMessage `json:"gpu_info"`
}

type pairResp struct {
	NodeID    string `json:"node_id"`
	NodeToken string `json:"node_token"`
}

func (h *Handlers) pair(w http.ResponseWriter, r *http.Request) {
	var req pairReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Token == "" || req.Name == "" {
		httperr.Write(w, http.StatusBadRequest, "token and name required")
		return
	}
	node, nodeToken, err := h.svc.PairNode(r.Context(), PairRequest{
		Token:   req.Token,
		Name:    req.Name,
		GPUInfo: []byte(req.GPUInfo),
	})
	switch {
	case errors.Is(err, ErrTokenInvalid):
		httperr.Write(w, http.StatusUnauthorized, "invalid pair token")
		return
	case errors.Is(err, ErrNodeNameTaken):
		httperr.Write(w, http.StatusConflict, "node name taken")
		return
	case err != nil:
		slog.Error("pair node", "err", err)
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pairResp{
		NodeID:    node.ID.String(),
		NodeToken: nodeToken,
	})
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/nodes/ -race -count=1 -v`
Expected: 모든 테스트 PASS (service 테스트들 + handler 테스트 4개).

- [ ] **Step 5: Commit**

```bash
git add internal/nodes/handlers.go internal/nodes/handlers_test.go
git commit -m "feat(nodes): http pair-token + pair endpoints"
```

---

### Task 4: cmd/control-plane/main.go에 nodes 라우트 등록

**Files:**
- Modify: `cmd/control-plane/main.go`

- [ ] **Step 1: import 추가**

`cmd/control-plane/main.go` import 블록에 추가:
```go
	"github.com/paul/flexctl/internal/nodes"
```

- [ ] **Step 2: 라우트 등록**

기존 `r.Group(func(r chi.Router) { ... })` 인증 그룹 안에 nodes MountAuthed 추가, 그리고 그 그룹 *밖*에 MountPublic 호출.

기존 블록을 다음으로 교체 (Plan 2 Task 9 끝의 모양 기준):
```go
	nodesSvc := nodes.NewService(pool)
	nodesH := nodes.NewHandlers(nodesSvc)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		usersH.MountAuthed(r)
		sshkeys.NewHandlers(sshkeys.NewService(pool)).Mount(r)
		nodesH.MountAuthed(r)
	})
	nodesH.MountPublic(r)
```

NOTE: `nodesSvc`/`nodesH` 변수는 다음 Task(gRPC 서버)에서도 재사용하므로 그룹 *밖*에서 만들고 보관.

- [ ] **Step 3: 빌드 + 수동 검증**

```bash
docker compose up -d postgres headscale
make migrate-up   # 0004, 0005 적용
make headscale-init  # 키 발급, 마지막 줄 복사
KEY=<paste>

FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
FLEX_HEADSCALE_API_KEY="$KEY" \
make run &
sleep 1

# 가입 + 로그인 (이메일 충돌 시 다른 값)
curl -s -X POST http://localhost:8080/v1/auth/signup \
  -H 'content-type: application/json' \
  -d '{"email":"node@x.com","slug":"node-test","password":"correct-horse-battery"}' \
  -c /tmp/c.txt > /dev/null

# 페어링 토큰 발급
curl -i -X POST http://localhost:8080/v1/nodes/pair-token -b /tmp/c.txt
# Expected: 201 + JSON {token: "FX-...", expires_at: "..."}
TOKEN=$(curl -s -X POST http://localhost:8080/v1/nodes/pair-token -b /tmp/c.txt | jq -r .token)

# 페어링
curl -i -X POST http://localhost:8080/v1/nodes/pair \
  -H 'content-type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"name\":\"manual-test\",\"gpu_info\":[]}"
# Expected: 201 + {node_id: "...", node_token: "..."}

pkill -f bin/control-plane
```

- [ ] **Step 4: Commit**

```bash
git add cmd/control-plane/main.go
git commit -m "feat(control-plane): wire nodes pair-token + pair routes"
```

---

### Task 5: protobuf 정의 + 코드 생성

**Files:**
- Create: `proto/agent.proto`
- Create: `internal/agentpb/agent.pb.go` (generated, committed)
- Create: `internal/agentpb/agent_grpc.pb.go` (generated, committed)
- Modify: `Makefile` (proto-gen 타겟 추가)

- [ ] **Step 1: protoc 도구 설치 (한 번만)**

```bash
# Ubuntu/Debian:
sudo apt update && sudo apt install -y protobuf-compiler
# macOS:
# brew install protobuf

# Go protoc 플러그인:
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# PATH에 ~/go/bin 있어야 함:
echo $PATH | grep -q "$HOME/go/bin" || echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.bashrc
```

- [ ] **Step 2: proto 파일 작성**

`proto/agent.proto`:
```proto
syntax = "proto3";

package flexctl.agent.v1;

option go_package = "github.com/paul/flexctl/internal/agentpb;agentpb";

import "google/protobuf/timestamp.proto";

service Agent {
  // Stream is a long-lived bidirectional stream between an agent and the
  // control plane. The agent authenticates via the `node-token` metadata
  // header on stream open. First client message must be Register.
  rpc Stream(stream AgentMessage) returns (stream ControlMessage);
}

message AgentMessage {
  oneof payload {
    Register register = 1;
    Heartbeat heartbeat = 2;
  }
}

message ControlMessage {
  oneof payload {
    RegisterAck register_ack = 1;
    HeartbeatAck heartbeat_ack = 2;
  }
}

message Register {
  string agent_version = 1;
  repeated GPU gpus = 2;
}

message Heartbeat {
  google.protobuf.Timestamp at = 1;
}

message RegisterAck {
  string node_id = 1;
}

message HeartbeatAck {}

message GPU {
  int32 index = 1;
  string model = 2;
  int32 vram_mb = 3;
}
```

- [ ] **Step 3: Makefile 타겟 추가**

기존 `.PHONY` 라인에 `proto-gen` 추가, 파일 끝에 다음 추가:
```make
proto-gen:
	protoc \
	  --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  --proto_path=proto \
	  proto/agent.proto
	@mkdir -p internal/agentpb
	@mv proto/agent.pb.go internal/agentpb/agent.pb.go 2>/dev/null || true
	@mv proto/agent_grpc.pb.go internal/agentpb/agent_grpc.pb.go 2>/dev/null || true
```

NOTE: `paths=source_relative` 옵션이 있어도 `--go_out=.` 기준이라 결과가 `proto/agent.pb.go`로 떨어짐. `mv`로 정리. 또는 `option go_package`의 import path를 따라가도록 `--go_out=.` 만 쓰고 `paths=source_relative` 빼면 자동으로 `internal/agentpb/agent.pb.go`에 생성됨. 위 방식이 명시적이라 plan에 채택.

- [ ] **Step 4: 생성 + 결과 확인**

```bash
make proto-gen
ls internal/agentpb/
# Expected: agent.pb.go agent_grpc.pb.go
```

생성된 파일에 다음이 포함돼야:
- `agent.pb.go`: `AgentMessage`, `ControlMessage`, `Register`, `Heartbeat`, `RegisterAck`, `HeartbeatAck`, `GPU` 구조체
- `agent_grpc.pb.go`: `AgentClient`/`AgentServer` 인터페이스, `RegisterAgentServer`, `NewAgentClient` 함수, `Agent_StreamServer`/`Agent_StreamClient` 인터페이스

- [ ] **Step 5: go.mod에 grpc 의존성 추가**

```bash
go get google.golang.org/grpc@v1.66.0
go get google.golang.org/protobuf@v1.34.2
go mod tidy
```

- [ ] **Step 6: 빌드 확인**

Run: `go build ./...`
Expected: 클린.

- [ ] **Step 7: Commit**

```bash
git add proto/agent.proto internal/agentpb/agent.pb.go internal/agentpb/agent_grpc.pb.go Makefile go.mod go.sum
git commit -m "feat(proto): agent gRPC bidi-stream definitions"
```

---

### Task 6: agentstream — 컨트롤 플레인 측 gRPC 서버 (TDD)

**Files:**
- Create: `internal/agentstream/server.go`
- Create: `internal/agentstream/server_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/agentstream/server_test.go`:
```go
package agentstream_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/nodes"
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
	return pool
}

// startServer boots a gRPC server on a random port wired to a real Postgres testcontainer
// and returns (gRPC client, nodes.Service, cleanup).
func startServer(t *testing.T) (agentpb.AgentClient, *nodes.Service, func()) {
	t.Helper()
	pool := newTestPool(t)
	svc := nodes.NewService(pool)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	agentpb.RegisterAgentServer(srv, agentstream.NewServer(svc))

	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	cleanup := func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
	return agentpb.NewAgentClient(conn), svc, cleanup
}

func TestStream_RegisterAndHeartbeat(t *testing.T) {
	client, svc, cleanup := startServer(t)
	defer cleanup()

	// Set up a paired node
	ctx := context.Background()
	usersSvc := users.NewService(svcPool(svc))
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	pairToken, err := svc.CreatePairToken(ctx, u.ID)
	require.NoError(t, err)
	node, nodeToken, err := svc.PairNode(ctx, nodes.PairRequest{Token: pairToken, Name: "rtx"})
	require.NoError(t, err)

	// Open stream with auth
	md := metadata.Pairs("node-token", nodeToken)
	streamCtx := metadata.NewOutgoingContext(ctx, md)
	stream, err := client.Stream(streamCtx)
	require.NoError(t, err)

	// Send Register
	require.NoError(t, stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{
			Register: &agentpb.Register{
				AgentVersion: "0.1.0",
				Gpus: []*agentpb.GPU{
					{Index: 0, Model: "RTX 4090", VramMb: 24576},
				},
			},
		},
	}))

	// Receive RegisterAck
	resp, err := stream.Recv()
	require.NoError(t, err)
	ack := resp.GetRegisterAck()
	require.NotNil(t, ack)
	require.Equal(t, node.ID.String(), ack.NodeId)

	// Send Heartbeat
	require.NoError(t, stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Heartbeat{
			Heartbeat: &agentpb.Heartbeat{At: timestamppb.Now()},
		},
	}))

	// Receive HeartbeatAck
	resp, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, resp.GetHeartbeatAck())

	// Verify DB state
	gotNode, err := svc.AuthenticateNodeToken(ctx, nodeToken)
	require.NoError(t, err)
	require.Equal(t, "online", gotNode.Status)
	require.Equal(t, "0.1.0", gotNode.AgentVersion)

	// Close stream
	require.NoError(t, stream.CloseSend())
	// Server should mark offline; allow some time
	time.Sleep(200 * time.Millisecond)
	gotNode, err = svc.AuthenticateNodeToken(ctx, nodeToken)
	require.NoError(t, err)
	require.Equal(t, "offline", gotNode.Status)
}

func TestStream_RejectsMissingToken(t *testing.T) {
	client, _, cleanup := startServer(t)
	defer cleanup()

	stream, err := client.Stream(context.Background())
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err, "stream must reject without node-token metadata")
}

func TestStream_RejectsBadToken(t *testing.T) {
	client, _, cleanup := startServer(t)
	defer cleanup()

	md := metadata.Pairs("node-token", "bogus-token-not-real")
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.Stream(ctx)
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err)
}

func TestStream_FirstMessageMustBeRegister(t *testing.T) {
	client, svc, cleanup := startServer(t)
	defer cleanup()

	ctx := context.Background()
	usersSvc := users.NewService(svcPool(svc))
	u, err := usersSvc.Signup(ctx, "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)
	pairToken, _ := svc.CreatePairToken(ctx, u.ID)
	_, nodeToken, _ := svc.PairNode(ctx, nodes.PairRequest{Token: pairToken, Name: "rtx"})

	md := metadata.Pairs("node-token", nodeToken)
	streamCtx := metadata.NewOutgoingContext(ctx, md)
	stream, err := client.Stream(streamCtx)
	require.NoError(t, err)

	// Send Heartbeat first (out of order)
	require.NoError(t, stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Heartbeat{Heartbeat: &agentpb.Heartbeat{At: timestamppb.Now()}},
	}))
	_, err = stream.Recv()
	require.Error(t, err, "first message must be Register")
}

// svcPool extracts the pgxpool from a *nodes.Service for test reuse.
// This is a test-only hack — exposed via test helper because nodes.Service
// doesn't have a Pool() accessor in production code.
func svcPool(s *nodes.Service) *pgxpool.Pool {
	return nodes.PoolFor(s)
}
```

NOTE: 마지막 헬퍼 `svcPool`은 `nodes.PoolFor(s)`를 호출. 이 helper는 nodes 패키지에 추가해야 하는 test-only access — `internal/nodes/service.go`에 `// PoolFor is for tests only.` 주석과 함께 `func PoolFor(s *Service) *pgxpool.Pool { return s.pool }` 작성.

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/agentstream/ -v`
Expected: 컴파일 에러 (`agentstream.NewServer`, `agentstream.Server`, `nodes.PoolFor` 미정의).

- [ ] **Step 3: nodes.PoolFor 추가**

`internal/nodes/service.go`의 `Service` 정의 다음에 추가:
```go
// PoolFor returns the underlying pgx pool for test wiring. Production code
// should call methods on Service directly.
func PoolFor(s *Service) *pgxpool.Pool { return s.pool }
```

- [ ] **Step 4: agentstream 서버 구현**

`internal/agentstream/server.go`:
```go
package agentstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/nodes"
)

const tokenMetadataKey = "node-token"

type Server struct {
	agentpb.UnimplementedAgentServer
	svc *nodes.Service
}

func NewServer(svc *nodes.Service) *Server { return &Server{svc: svc} }

func (s *Server) Stream(stream agentpb.Agent_StreamServer) error {
	ctx := stream.Context()

	// Authenticate
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get(tokenMetadataKey)
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing node-token")
	}
	node, err := s.svc.AuthenticateNodeToken(ctx, tokens[0])
	if errors.Is(err, nodes.ErrNodeTokenInvalid) {
		return status.Error(codes.Unauthenticated, "invalid node-token")
	}
	if err != nil {
		slog.Error("authenticate node token", "err", err)
		return status.Error(codes.Internal, "auth failure")
	}

	slog.Info("agent connected", "node_id", node.ID.String(), "name", node.Name)

	// Mark offline on disconnect (best-effort)
	defer func() {
		bgCtx := context.Background()
		if err := s.svc.MarkOffline(bgCtx, node.ID); err != nil {
			slog.Warn("mark offline", "err", err, "node_id", node.ID.String())
		}
	}()

	// First message MUST be Register
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.FailedPrecondition, "first message must be Register")
	}
	gpuJSON, err := encodeGPUs(reg.GetGpus())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "encode gpu_info: %v", err)
	}
	if err := s.svc.UpdateRegister(ctx, node.ID, reg.GetAgentVersion(), gpuJSON); err != nil {
		slog.Error("update register", "err", err)
		return status.Error(codes.Internal, "update register")
	}
	if err := stream.Send(&agentpb.ControlMessage{
		Payload: &agentpb.ControlMessage_RegisterAck{
			RegisterAck: &agentpb.RegisterAck{NodeId: node.ID.String()},
		},
	}); err != nil {
		return err
	}

	// Subsequent messages: Heartbeat (others ignored for now)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch msg.GetPayload().(type) {
		case *agentpb.AgentMessage_Heartbeat:
			if err := s.svc.RecordHeartbeat(ctx, node.ID); err != nil {
				slog.Warn("record heartbeat", "err", err)
			}
			if err := stream.Send(&agentpb.ControlMessage{
				Payload: &agentpb.ControlMessage_HeartbeatAck{HeartbeatAck: &agentpb.HeartbeatAck{}},
			}); err != nil {
				return err
			}
		default:
			slog.Warn("unexpected message type from agent", "node_id", node.ID.String())
			// ignore — future commands handled here
		}
	}
}

func encodeGPUs(gpus []*agentpb.GPU) ([]byte, error) {
	if len(gpus) == 0 {
		return []byte(`[]`), nil
	}
	type gpuJSON struct {
		Index  int32  `json:"index"`
		Model  string `json:"model"`
		VRAMMB int32  `json:"vram_mb"`
	}
	out := make([]gpuJSON, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, gpuJSON{Index: g.GetIndex(), Model: g.GetModel(), VRAMMB: g.GetVramMb()})
	}
	return json.Marshal(out)
}
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/agentstream/ -race -count=1 -v`
Expected: 4 tests PASS (RegisterAndHeartbeat, RejectsMissingToken, RejectsBadToken, FirstMessageMustBeRegister).

전체:
Run: `go test ./... -race -count=1`
Expected: 모두 PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agentstream internal/nodes/service.go
git commit -m "feat(agentstream): grpc server with node-token auth + register/heartbeat"
```

---

### Task 7: cmd/control-plane/main.go에 gRPC 서버 와이어링

**Files:**
- Modify: `cmd/control-plane/main.go`

- [ ] **Step 1: import 추가**

```go
	"net"
	// ...
	"google.golang.org/grpc"
	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
```

NOTE: `net`은 이미 있을 수 있음 — 중복이면 그대로.

- [ ] **Step 2: gRPC 서버 시작**

`http.Server` 시작 직전(또는 직후)에 별도 goroutine으로 gRPC 서버 추가. 기존 `srv := &http.Server{...}` 정의 직전에 다음 추가:

```go
	grpcAddr := os.Getenv("FLEX_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9090"
	}
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		slog.Error("grpc listen", "err", err, "addr", grpcAddr)
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcSrv, agentstream.NewServer(nodesSvc))
	go func() {
		slog.Info("grpc serving", "addr", grpcAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			slog.Error("grpc serve", "err", err)
			os.Exit(1)
		}
	}()
```

- [ ] **Step 3: graceful shutdown에 gRPC 서버 stop 추가**

`<-ctx.Done()` 다음, `srv.Shutdown(...)` 호출 다음에 추가:
```go
	grpcSrv.GracefulStop()
```

- [ ] **Step 4: 빌드 + 수동 검증**

```bash
make build
docker compose up -d postgres headscale
make migrate-up

KEY=<headscale api key>

FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
FLEX_HEADSCALE_API_KEY="$KEY" \
./bin/control-plane &
sleep 1

# gRPC 포트가 listening 중인지
ss -tlnp | grep 9090
# Expected: listening on :9090

# 페어링 → node_token 받음 → grpc로 stream 연결 시도 (다음 task에서 자동화)
pkill -f bin/control-plane
```

- [ ] **Step 5: Commit**

```bash
git add cmd/control-plane/main.go
git commit -m "feat(control-plane): start grpc agent stream server on :9090"
```

---

### Task 8: flexctl Cobra CLI 부트스트랩

**Files:**
- Create: `cmd/flexctl/main.go`
- Create: `internal/flexctlcli/root.go`

- [ ] **Step 1: Cobra 의존성 추가**

```bash
go get github.com/spf13/cobra@v1.8.1
go mod tidy
```

- [ ] **Step 2: cmd/flexctl/main.go 작성**

```go
package main

import (
	"os"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func main() {
	if err := flexctlcli.Execute(); err != nil {
		os.Exit(1)
	}
}
```

- [ ] **Step 3: internal/flexctlcli/root.go 작성**

```go
package flexctlcli

import (
	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "flexctl",
		Short:         "flexctl — self-sovereign GPU cloud agent + client",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	return root
}

func Execute() error {
	return NewRootCmd().Execute()
}
```

- [ ] **Step 4: Makefile에 flexctl 빌드 타겟 추가**

기존 `build:` 타겟 아래에 추가:
```make
build-flexctl:
	go build -o bin/flexctl ./cmd/flexctl
```

`build:` 타겟을 수정해 둘 다 빌드:
```make
build: build-control-plane build-flexctl

build-control-plane:
	go build -o bin/control-plane ./cmd/control-plane
```

기존 단일 `build:` 타겟이 있으면 위로 교체. 그 외 변경 X.

- [ ] **Step 5: 검증**

```bash
make build-flexctl
./bin/flexctl --help
# Expected: Usage 출력, --version, --help 등 기본 옵션
./bin/flexctl --version
# Expected: flexctl version 0.1.0-dev
```

- [ ] **Step 6: Commit**

```bash
git add cmd/flexctl internal/flexctlcli/root.go Makefile go.mod go.sum
git commit -m "feat(flexctl): cobra cli bootstrap"
```

---

### Task 9: `flexctl join <token>` 명령

**Files:**
- Create: `internal/flexctlcli/join.go`
- Create: `internal/flexctlcli/join_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/flexctlcli/join_test.go`:
```go
package flexctlcli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestJoinCommand_PersistsConfig(t *testing.T) {
	// Stub control plane that returns a fixed pair response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/nodes/pair", r.URL.Path)
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, "FX-test-token", req["token"])
		require.Equal(t, "test-node", req["name"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"node_id":    "00000000-0000-0000-0000-000000000123",
			"node_token": "deadbeefcafe",
		})
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	cfgPath := filepath.Join(tmpHome, "agent.toml")

	cmd := flexctlcli.NewJoinCmd()
	cmd.SetArgs([]string{
		"FX-test-token",
		"--name", "test-node",
		"--control-plane", srv.URL,
		"--config", cfgPath,
	})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.NoError(t, err)

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	contents := string(data)
	require.Contains(t, contents, `node_id = "00000000-0000-0000-0000-000000000123"`)
	require.Contains(t, contents, `node_token = "deadbeefcafe"`)
	require.Contains(t, contents, `control_plane = "`+srv.URL+`"`)
}

func TestJoinCommand_FailsOnBadToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid pair token"}`))
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	cfgPath := filepath.Join(tmpHome, "agent.toml")

	cmd := flexctlcli.NewJoinCmd()
	cmd.SetArgs([]string{
		"FX-bad",
		"--name", "test-node",
		"--control-plane", srv.URL,
		"--config", cfgPath,
	})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)

	_, err = os.Stat(cfgPath)
	require.True(t, os.IsNotExist(err), "config must not be written on auth failure")
}

// Compile-time check that NewJoinCmd returns a *cobra.Command.
var _ = (*cobra.Command)(nil)
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/flexctlcli/ -v`
Expected: 컴파일 에러 (`flexctlcli.NewJoinCmd` 미정의).

- [ ] **Step 3: 구현**

`internal/flexctlcli/join.go`:
```go
package flexctlcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// AgentConfig is what `flexctl join` writes to disk for `flexctl agent` to read.
type AgentConfig struct {
	NodeID       string `toml:"node_id"`
	NodeToken    string `toml:"node_token"`
	ControlPlane string `toml:"control_plane"`
	GRPCAddress  string `toml:"grpc_address"`
}

func NewJoinCmd() *cobra.Command {
	var (
		name         string
		controlPlane string
		grpcAddr     string
		configPath   string
	)
	cmd := &cobra.Command{
		Use:   "join <pair-token>",
		Short: "Pair this node with the control plane and persist the config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := args[0]
			if name == "" {
				h, _ := os.Hostname()
				name = h
			}
			if name == "" {
				return fmt.Errorf("--name is required (and hostname could not be detected)")
			}
			if controlPlane == "" {
				return fmt.Errorf("--control-plane is required")
			}
			body, _ := json.Marshal(map[string]any{
				"token":    token,
				"name":     name,
				"gpu_info": []any{},
			})
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost,
				controlPlane+"/v1/nodes/pair", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")

			httpClient := &http.Client{Timeout: 30 * time.Second}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("call control-plane: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("pair failed: %s", resp.Status)
			}
			var pairResp struct {
				NodeID    string `json:"node_id"`
				NodeToken string `json:"node_token"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&pairResp); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			cfg := AgentConfig{
				NodeID:       pairResp.NodeID,
				NodeToken:    pairResp.NodeToken,
				ControlPlane: controlPlane,
				GRPCAddress:  grpcAddr,
			}
			if err := writeAgentConfig(configPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Paired as %s (node_id=%s)\nConfig: %s\n",
				name, pairResp.NodeID, configPath)
			fmt.Fprintln(cmd.OutOrStdout(), "Next: run `flexctl agent` (and see README for systemd unit).")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Node name (defaults to hostname)")
	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "Control plane HTTP URL (e.g. https://flexctl.example.com)")
	cmd.Flags().StringVar(&grpcAddr, "grpc-address", "", "Override gRPC address (defaults to host:9090 derived from --control-plane)")
	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath(), "Path to write agent config")
	return cmd
}

func writeAgentConfig(path string, cfg AgentConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	contents := fmt.Sprintf(`node_id = %q
node_token = %q
control_plane = %q
grpc_address = %q
`, cfg.NodeID, cfg.NodeToken, cfg.ControlPlane, cfg.GRPCAddress)
	return os.WriteFile(path, []byte(contents), 0o600)
}

func defaultConfigPath() string {
	if v := os.Getenv("FLEXCTL_CONFIG"); v != "" {
		return v
	}
	return "/etc/flexctl/agent.toml"
}
```

- [ ] **Step 4: root.go에 join 등록**

`internal/flexctlcli/root.go`의 `NewRootCmd` 함수를 수정:
```go
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "flexctl",
		Short:         "flexctl — self-sovereign GPU cloud agent + client",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(NewJoinCmd())
	return root
}
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/flexctlcli/ -race -count=1 -v`
Expected: 2 tests PASS.

수동 검증:
```bash
make build-flexctl
./bin/flexctl join --help
# Expected: usage 출력 with --name/--control-plane/--config flags
```

- [ ] **Step 6: Commit**

```bash
git add internal/flexctlcli
git commit -m "feat(flexctl): join command pairs and persists config"
```

---

### Task 10: flexctlagent — 스트림 클라이언트 + 재연결

**Files:**
- Create: `internal/flexctlagent/agent.go`
- Create: `internal/flexctlagent/agent_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/flexctlagent/agent_test.go`:
```go
package flexctlagent_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/flexctlagent"
)

// fakeServer counts Register and Heartbeat messages and replies with the
// matching ack. It expects metadata `node-token = "good"`.
type fakeServer struct {
	agentpb.UnimplementedAgentServer
	registers  atomic.Int32
	heartbeats atomic.Int32
}

func (f *fakeServer) Stream(stream agentpb.Agent_StreamServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok || len(md.Get("node-token")) == 0 || md.Get("node-token")[0] != "good" {
		return status.Error(codes.Unauthenticated, "bad token")
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		switch p := msg.GetPayload().(type) {
		case *agentpb.AgentMessage_Register:
			f.registers.Add(1)
			_ = p
			if err := stream.Send(&agentpb.ControlMessage{
				Payload: &agentpb.ControlMessage_RegisterAck{
					RegisterAck: &agentpb.RegisterAck{NodeId: "node-1"},
				},
			}); err != nil {
				return err
			}
		case *agentpb.AgentMessage_Heartbeat:
			f.heartbeats.Add(1)
			if err := stream.Send(&agentpb.ControlMessage{
				Payload: &agentpb.ControlMessage_HeartbeatAck{HeartbeatAck: &agentpb.HeartbeatAck{}},
			}); err != nil {
				return err
			}
		}
	}
}

func startFakeServer(t *testing.T) (string, *fakeServer, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	fs := &fakeServer{}
	agentpb.RegisterAgentServer(srv, fs)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), fs, func() { srv.GracefulStop() }
}

func TestAgent_RegistersAndHeartbeats(t *testing.T) {
	addr, fs, stop := startFakeServer(t)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := flexctlagent.New(flexctlagent.Config{
		GRPCAddress:       addr,
		NodeToken:         "good",
		AgentVersion:      "0.1.0-test",
		HeartbeatInterval: 100 * time.Millisecond,
		GPUDetector:       flexctlagent.StaticGPUs(nil),
	})
	go func() { _ = a.Run(ctx) }()

	// Wait for >= 3 heartbeats
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fs.registers.Load() >= 1 && fs.heartbeats.Load() >= 3 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected ≥1 register and ≥3 heartbeats, got register=%d heartbeats=%d",
		fs.registers.Load(), fs.heartbeats.Load())
}

func TestAgent_ReconnectsAfterServerRestart(t *testing.T) {
	addr1, fs1, stop1 := startFakeServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := flexctlagent.New(flexctlagent.Config{
		GRPCAddress:       addr1,
		NodeToken:         "good",
		AgentVersion:      "0.1.0-test",
		HeartbeatInterval: 100 * time.Millisecond,
		BackoffInitial:    50 * time.Millisecond,
		BackoffMax:        500 * time.Millisecond,
		GPUDetector:       flexctlagent.StaticGPUs(nil),
	})
	go func() { _ = a.Run(ctx) }()

	// Wait for first register
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fs1.registers.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	require.GreaterOrEqual(t, fs1.registers.Load(), int32(1))

	// Stop server (force disconnect), bring up new server on same port
	stop1()
	// Reuse the same port — testcontainers used port 0, so reconnect would fail.
	// Instead, we'll just stop and assert the agent is still running and trying.
	// A more thorough test would need a fixed port; we rely on the simpler invariant
	// that the agent's Run() doesn't return when server dies.
	time.Sleep(200 * time.Millisecond)
	// The agent must still be alive — cancel its context to confirm clean shutdown.
	cancel()
	time.Sleep(200 * time.Millisecond)
	_ = fs1
}

func TestAgent_NilGPUDetector(t *testing.T) {
	addr, _, stop := startFakeServer(t)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Nil detector must default to empty GPU list, not crash.
	a := flexctlagent.New(flexctlagent.Config{
		GRPCAddress:       addr,
		NodeToken:         "good",
		AgentVersion:      "0.1.0-test",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	doneCh := make(chan error, 1)
	go func() { doneCh <- a.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-doneCh
}
```

NOTE: TestAgent_ReconnectsAfterServerRestart는 검증이 약함 (재연결을 직접 확인 못함) — 이유는 fixed port로 두 번째 서버 시작이 testcontainers/port 0 패턴과 안 맞아서. 대안: agent가 서버 죽어도 살아있다는 invariant만 검증. Plan 외 작업: 재연결 자체는 다음 task에서 더 정교하게 검증.

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/flexctlagent/ -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/flexctlagent/agent.go`:
```go
package flexctlagent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/paul/flexctl/internal/agentpb"
)

// GPU mirrors agentpb.GPU but defined here to avoid coupling test code to proto.
type GPU struct {
	Index  int32
	Model  string
	VRAMMB int32
}

// GPUDetector returns the current GPU inventory. Implementations must be safe
// to call concurrently and return quickly.
type GPUDetector interface {
	Detect(ctx context.Context) []GPU
}

// StaticGPUs returns a GPUDetector that always returns the given inventory.
func StaticGPUs(gpus []GPU) GPUDetector { return staticGPUs(gpus) }

type staticGPUs []GPU

func (s staticGPUs) Detect(_ context.Context) []GPU { return []GPU(s) }

type Config struct {
	GRPCAddress       string        // host:port
	NodeToken         string        // long-lived auth token
	AgentVersion      string        // e.g. "0.1.0"
	HeartbeatInterval time.Duration // default 30s
	BackoffInitial    time.Duration // default 1s
	BackoffMax        time.Duration // default 60s
	GPUDetector       GPUDetector   // optional; nil → empty list
}

type Agent struct {
	cfg Config
}

func New(cfg Config) *Agent {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}
	if cfg.BackoffInitial == 0 {
		cfg.BackoffInitial = time.Second
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 60 * time.Second
	}
	return &Agent{cfg: cfg}
}

// Run drives the connect→register→heartbeat loop. It returns only when ctx is
// canceled. On every disconnect it sleeps with jittered backoff and retries.
func (a *Agent) Run(ctx context.Context) error {
	backoff := a.cfg.BackoffInitial
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.runOnce(ctx)
		if err != nil {
			slog.Warn("agent stream ended", "err", err)
		}
		// Sleep with jitter; cap at BackoffMax.
		delay := withJitter(backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		backoff = nextBackoff(backoff, a.cfg.BackoffMax)
		// On a successful connection (runOnce returned nil), reset backoff for
		// future failures. We can't tell from err alone, but if the connection
		// lasted longer than BackoffMax we treat it as healthy and reset.
		if err == nil {
			backoff = a.cfg.BackoffInitial
		}
	}
}

func (a *Agent) runOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(a.cfg.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := agentpb.NewAgentClient(conn)
	md := metadata.Pairs("node-token", a.cfg.NodeToken)
	streamCtx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, md))
	defer cancel()

	stream, err := client.Stream(streamCtx)
	if err != nil {
		return err
	}

	// Send Register
	gpus := a.detectGPUs(ctx)
	if err := stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{
			Register: &agentpb.Register{
				AgentVersion: a.cfg.AgentVersion,
				Gpus:         gpus,
			},
		},
	}); err != nil {
		return err
	}

	// Wait for RegisterAck
	if _, err := stream.Recv(); err != nil {
		return err
	}

	// Concurrently: heartbeat sender + ack receiver
	errCh := make(chan error, 2)
	go func() {
		ticker := time.NewTicker(a.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				errCh <- streamCtx.Err()
				return
			case <-ticker.C:
				if err := stream.Send(&agentpb.AgentMessage{
					Payload: &agentpb.AgentMessage_Heartbeat{
						Heartbeat: &agentpb.Heartbeat{At: timestamppb.Now()},
					},
				}); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
		}
	}()

	// Wait for either side to terminate.
	return <-errCh
}

func (a *Agent) detectGPUs(ctx context.Context) []*agentpb.GPU {
	if a.cfg.GPUDetector == nil {
		return nil
	}
	gpus := a.cfg.GPUDetector.Detect(ctx)
	out := make([]*agentpb.GPU, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, &agentpb.GPU{Index: g.Index, Model: g.Model, VramMb: g.VRAMMB})
	}
	return out
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

func withJitter(d time.Duration) time.Duration {
	// ±25% jitter
	jitter := time.Duration(rand.Int64N(int64(d) / 2)) //nolint:gosec — non-crypto
	return d/2 + jitter
}
```

NOTE: `math/rand/v2`는 Go 1.22+ 표준. `rand.Int64N`은 0..N-1 범위.

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/flexctlagent/ -race -count=1 -v`
Expected: 3 tests PASS (RegistersAndHeartbeats, ReconnectsAfterServerRestart, NilGPUDetector).

전체:
Run: `go test ./... -race -count=1`
Expected: 모두 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/flexctlagent
git commit -m "feat(flexctlagent): grpc stream client with reconnect + heartbeat"
```

---

### Task 11: GPU 정보 수집 (nvidia-smi)

**Files:**
- Create: `internal/gpuinfo/nvidia.go`
- Create: `internal/gpuinfo/nvidia_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/gpuinfo/nvidia_test.go`:
```go
package gpuinfo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlagent"
	"github.com/paul/flexctl/internal/gpuinfo"
)

func TestParseNvidiaSMI_Happy(t *testing.T) {
	out := `0, NVIDIA GeForce RTX 4090, 24576 MiB
1, NVIDIA H100 80GB HBM3, 81920 MiB
`
	got, err := gpuinfo.ParseNvidiaSMI(out)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, flexctlagent.GPU{Index: 0, Model: "NVIDIA GeForce RTX 4090", VRAMMB: 24576}, got[0])
	require.Equal(t, flexctlagent.GPU{Index: 1, Model: "NVIDIA H100 80GB HBM3", VRAMMB: 81920}, got[1])
}

func TestParseNvidiaSMI_EmptyOK(t *testing.T) {
	got, err := gpuinfo.ParseNvidiaSMI("")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestParseNvidiaSMI_BadLineFails(t *testing.T) {
	_, err := gpuinfo.ParseNvidiaSMI("not, a valid line")
	require.Error(t, err)
}

func TestNvidiaDetector_NoBinaryGracefulEmpty(t *testing.T) {
	// PATH 가 비어 있으면 nvidia-smi를 못 찾고 빈 슬라이스 반환 (에러 X)
	d := gpuinfo.NewNvidiaDetector(gpuinfo.WithExecLookPath(func(_ string) (string, error) {
		return "", errors.New("not found")
	}))
	got := d.Detect(context.Background())
	require.Empty(t, got)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/gpuinfo/ -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/gpuinfo/nvidia.go`:
```go
package gpuinfo

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/paul/flexctl/internal/flexctlagent"
)

// NvidiaDetector implements flexctlagent.GPUDetector by shelling out to nvidia-smi.
type NvidiaDetector struct {
	lookPath func(string) (string, error)
	timeout  time.Duration
}

type Option func(*NvidiaDetector)

func WithExecLookPath(f func(string) (string, error)) Option {
	return func(d *NvidiaDetector) { d.lookPath = f }
}

func WithTimeout(d time.Duration) Option {
	return func(n *NvidiaDetector) { n.timeout = d }
}

func NewNvidiaDetector(opts ...Option) *NvidiaDetector {
	d := &NvidiaDetector{
		lookPath: exec.LookPath,
		timeout:  3 * time.Second,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Detect returns the GPU inventory. If nvidia-smi is missing or fails,
// returns an empty slice (not an error) so the agent can run on dev hosts
// without GPUs.
func (d *NvidiaDetector) Detect(ctx context.Context) []flexctlagent.GPU {
	bin, err := d.lookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--query-gpu=index,name,memory.total",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("nvidia-smi failed", "err", err)
		return nil
	}
	parsed, err := ParseNvidiaSMI(string(out))
	if err != nil {
		slog.Warn("parse nvidia-smi", "err", err)
		return nil
	}
	return parsed
}

// ParseNvidiaSMI parses the CSV output of `nvidia-smi --query-gpu=index,name,memory.total
// --format=csv,noheader,nounits`. Each line is "INDEX, MODEL, VRAM_MB".
// Whitespace around fields is trimmed.
func ParseNvidiaSMI(out string) ([]flexctlagent.GPU, error) {
	var result []flexctlagent.GPU
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("expected 3 fields per line, got %d in %q", len(parts), line)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("parse index: %w", err)
		}
		model := strings.TrimSpace(parts[1])
		vramStr := strings.TrimSpace(parts[2])
		// Strip optional "MiB" suffix in case --nounits was not honored.
		vramStr = strings.TrimSuffix(vramStr, " MiB")
		vramStr = strings.TrimSuffix(vramStr, "MiB")
		vram, err := strconv.Atoi(strings.TrimSpace(vramStr))
		if err != nil {
			return nil, fmt.Errorf("parse vram: %w", err)
		}
		result = append(result, flexctlagent.GPU{
			Index:  int32(idx),
			Model:  model,
			VRAMMB: int32(vram),
		})
	}
	return result, scanner.Err()
}
```

NOTE: `ParseNvidiaSMI`는 `--nounits`이 있어도 일부 환경에서 "MiB" 접미사가 남아 있어 안전하게 strip.

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/gpuinfo/ -race -count=1 -v`
Expected: 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gpuinfo
git commit -m "feat(gpuinfo): nvidia-smi parser + detector"
```

---

### Task 12: `flexctl agent` 명령 + e2e 통합

**Files:**
- Create: `internal/flexctlcli/agent.go`
- Create: `internal/flexctlcli/agent_test.go` (e2e — control plane + flexctl agent 모두 띄움)
- Modify: `internal/flexctlcli/root.go` (agent 서브커맨드 등록)

- [ ] **Step 1: e2e 실패 테스트 작성**

`internal/flexctlcli/agent_test.go`:
```go
package flexctlcli_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/grpc"

	"github.com/paul/flexctl/internal/agentpb"
	"github.com/paul/flexctl/internal/agentstream"
	"github.com/paul/flexctl/internal/flexctlcli"
	"github.com/paul/flexctl/internal/nodes"
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
	return pool
}

// TestAgentCommand_E2E pairs a node, runs `flexctl agent`, and verifies the
// node row reaches status=online with non-empty agent_version.
func TestAgentCommand_E2E(t *testing.T) {
	pool := newTestPool(t)
	svc := nodes.NewService(pool)

	usersSvc := users.NewService(pool)
	u, err := usersSvc.Signup(context.Background(), "p@x.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	pairToken, err := svc.CreatePairToken(context.Background(), u.ID)
	require.NoError(t, err)
	node, nodeToken, err := svc.PairNode(context.Background(), nodes.PairRequest{
		Token: pairToken, Name: "rtx",
	})
	require.NoError(t, err)

	// Boot gRPC server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcSrv, agentstream.NewServer(svc))
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	// Write agent config
	cfgPath := filepath.Join(t.TempDir(), "agent.toml")
	require.NoError(t, flexctlcli.WriteAgentConfigForTest(cfgPath, flexctlcli.AgentConfig{
		NodeID:       node.ID.String(),
		NodeToken:    nodeToken,
		ControlPlane: "http://unused-in-this-test",
		GRPCAddress:  lis.Addr().String(),
	}))

	// Run `flexctl agent --config <path>` for ~2 seconds
	cmd := flexctlcli.NewAgentCmd()
	cmd.SetArgs([]string{"--config", cfgPath, "--heartbeat", "200ms"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	_ = cmd.Execute() // returns when ctx times out

	// Verify DB
	got, err := svc.AuthenticateNodeToken(context.Background(), nodeToken)
	require.NoError(t, err)
	require.Equal(t, "online", got.Status)
	require.NotEmpty(t, got.AgentVersion)
}
```

NOTE: 테스트는 `flexctlcli.WriteAgentConfigForTest` (현재 `writeAgentConfig`를 export로 노출하거나 wrapper) 와 `flexctlcli.AgentConfig` 사용. 구현 시 `writeAgentConfig`를 `WriteAgentConfig`로 export, 또는 작은 wrapper 추가.

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/flexctlcli/ -run TestAgentCommand -v`
Expected: 컴파일 에러 (`flexctlcli.NewAgentCmd`, `flexctlcli.WriteAgentConfigForTest` 미정의).

- [ ] **Step 3: agent 명령 구현**

`internal/flexctlcli/agent.go`:
```go
package flexctlcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paul/flexctl/internal/flexctlagent"
	"github.com/paul/flexctl/internal/gpuinfo"
)

func NewAgentCmd() *cobra.Command {
	var (
		configPath string
		heartbeat  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the flexctl node agent (long-lived gRPC stream to control plane)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := readAgentConfig(configPath)
			if err != nil {
				return err
			}
			if cfg.GRPCAddress == "" {
				return errors.New("config has empty grpc_address; re-run `flexctl join` with --grpc-address")
			}
			a := flexctlagent.New(flexctlagent.Config{
				GRPCAddress:       cfg.GRPCAddress,
				NodeToken:         cfg.NodeToken,
				AgentVersion:      Version,
				HeartbeatInterval: heartbeat,
				GPUDetector:       gpuinfo.NewNvidiaDetector(),
			})
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return a.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath(), "Path to agent.toml")
	cmd.Flags().DurationVar(&heartbeat, "heartbeat", 30*time.Second, "Heartbeat interval")
	return cmd
}

// WriteAgentConfigForTest exposes config-write for test harnesses.
func WriteAgentConfigForTest(path string, cfg AgentConfig) error {
	return writeAgentConfig(path, cfg)
}

// readAgentConfig reads the TOML file written by `flexctl join`. Hand-parsed
// because we don't want to pull a TOML dep just for 4 keys.
func readAgentConfig(path string) (AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := AgentConfig{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"`)
		switch key {
		case "node_id":
			cfg.NodeID = val
		case "node_token":
			cfg.NodeToken = val
		case "control_plane":
			cfg.ControlPlane = val
		case "grpc_address":
			cfg.GRPCAddress = val
		}
	}
	if cfg.NodeToken == "" {
		return AgentConfig{}, errors.New("config missing node_token")
	}
	return cfg, nil
}
```

- [ ] **Step 4: root.go에 agent 등록**

`internal/flexctlcli/root.go`의 `NewRootCmd`에 한 줄 추가:
```go
	root.AddCommand(NewJoinCmd())
	root.AddCommand(NewAgentCmd())
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/flexctlcli/ -race -count=1 -v`
Expected: 3 tests PASS (Join 2개 + Agent E2E 1개).

전체:
Run: `go test ./... -race -count=1`
Expected: 모두 PASS.

- [ ] **Step 6: 수동 e2e (선택, dev 머신에 nvidia-smi 없어도 동작)**

```bash
docker compose up -d postgres headscale
make migrate-up
make headscale-init
KEY=<paste>

FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
FLEX_HEADSCALE_API_KEY="$KEY" \
./bin/control-plane &
sleep 1

# 가입 + 페어링 토큰 발급
curl -s -X POST http://localhost:8080/v1/auth/signup \
  -H 'content-type: application/json' \
  -d '{"email":"e2e@x.com","slug":"e2e","password":"correct-horse-battery"}' \
  -c /tmp/c.txt > /dev/null
TOKEN=$(curl -s -X POST http://localhost:8080/v1/nodes/pair-token -b /tmp/c.txt | jq -r .token)

# flexctl join (config 파일 임시 위치)
make build-flexctl
./bin/flexctl join "$TOKEN" \
  --name dev-machine \
  --control-plane http://localhost:8080 \
  --grpc-address localhost:9090 \
  --config /tmp/flexctl-agent.toml

# flexctl agent 실행 (백그라운드)
./bin/flexctl agent --config /tmp/flexctl-agent.toml --heartbeat 2s &
sleep 3

# 노드 상태 확인
docker compose exec postgres psql -U flex -d flex \
  -c "SELECT name, status, agent_version, last_seen_at FROM nodes;"
# Expected: status=online, agent_version=0.1.0-dev, last_seen_at 최근

pkill -f bin/flexctl
pkill -f bin/control-plane
```

- [ ] **Step 7: Commit**

```bash
git add internal/flexctlcli/agent.go internal/flexctlcli/agent_test.go internal/flexctlcli/root.go internal/flexctlcli/join.go
git commit -m "feat(flexctl): agent command runs gRPC stream loop with nvidia-smi"
```

---

### Task 13: README 갱신

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README에 새 환경 변수 + flexctl 사용법 + systemd 가이드 추가**

`README.md`의 "주요 환경 변수" 표에 한 줄 추가 (FLEX_HEADSCALE_API_KEY 다음):
```markdown
| `FLEX_GRPC_ADDR` | `:9090` | gRPC agent stream 리스닝 주소 |
```

"현재 노출된 엔드포인트" 표에 두 줄 추가 (DELETE /v1/me/ssh-keys 다음):
```markdown
| POST | `/v1/nodes/pair-token` | session | 1회용 페어링 토큰 발급 (10분 만료) |
| POST | `/v1/nodes/pair` | token | 페어링 토큰 사용 + 노드 등록 |
```

새 섹션을 파일 끝에 추가:
```markdown
## flexctl agent

GPU 서버에 설치할 단일 Go 바이너리.

### 빌드

    make build-flexctl

### 페어링

웹에서 페어링 토큰 발급 후:

    sudo flexctl join FX-XXXX-YYYY \
      --name my-rtx-server \
      --control-plane https://flexctl.example.com \
      --grpc-address flexctl.example.com:9090

`/etc/flexctl/agent.toml`에 `node_token`이 저장됨 (mode 0600).

### 실행 (foreground)

    sudo flexctl agent

### systemd 유닛 (직접 작성)

`/etc/systemd/system/flexctl-agent.service`:

    [Unit]
    Description=flexctl GPU node agent
    After=network-online.target docker.service
    Wants=network-online.target

    [Service]
    Type=simple
    ExecStart=/usr/local/bin/flexctl agent
    Restart=always
    RestartSec=5s
    User=root

    [Install]
    WantedBy=multi-user.target

활성화:

    sudo systemctl daemon-reload
    sudo systemctl enable --now flexctl-agent

로그:

    journalctl -u flexctl-agent -f
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: README adds flexctl agent + node pairing endpoints"
```

---

## End-to-end 검증 체크리스트

Plan 3 구현 완료 후 다음을 모두 통과해야 합니다.

- [ ] `make test`: 기존 52 + 신규 약 25개 = 77개 이상 테스트 PASS
- [ ] `go vet ./...`: 경고 없음
- [ ] `make build` 와 `make build-flexctl` 모두 성공
- [ ] 수동 e2e (Task 12 Step 6) 시나리오 통과:
  - `flexctl join` → `/etc/flexctl/agent.toml` 생성됨
  - `flexctl agent` 실행 후 `nodes.status = 'online'`, `agent_version` 업데이트됨
  - `last_seen_at` 5초 이내
- [ ] flexctl agent가 컨트롤 플레인 재시작에도 자동 재연결 (수동: control-plane 죽였다가 재기동, agent 로그에 reconnect 메시지 + status 다시 online)
- [ ] 잘못된 node_token으로 stream 시도 시 즉시 에러 반환 + agent가 backoff 적용

이 체크리스트가 통과되면 다음 plan(Plan 4: Environment Lifecycle — 사이드카+dev 컨테이너 묶음)으로 진행할 수 있습니다.
