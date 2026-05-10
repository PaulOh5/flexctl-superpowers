# Headscale Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Headscale을 셀프 호스팅 사이드카로 운영하면서 컨트롤 플레인이 사용자 가입 시 Headscale 사용자/태그를 자동 동기화하도록 만든다. 끝나면 통합 테스트로 "사용자가 가입 → Headscale에 같은 slug의 사용자 생성 + ACL JSON에 그 사용자 태그 라인이 들어옴"을 검증할 수 있다.

**Architecture:** Headscale을 docker-compose 사이드카로 추가, sqlite 백엔드로 단순 운영. 내부 패키지 두 개 — `internal/headscale`(HTTP 클라이언트 + ACL 생성기) + `internal/policy`(DB 상태 ↔ Headscale 동기화). 사용자 가입 hook을 `users` 패키지에 도입(인터페이스 + no-op 기본값) → 컨트롤 플레인 main.go에서 실제 policy 구현 주입. Headscale API key는 일회 생성해서 환경 변수로 주입. Plan 1에 이미 wire되어 있는 패턴(서비스 → 핸들러 → main 조립) 그대로 확장.

**Tech Stack:** Go 1.22+, Headscale 0.23.0, testcontainers-go, 기존 chi/pgx 그대로.

**Out of scope (다음 plan):** pre-auth key 발급(노드/환경 단위 — Plan 3-4), 노드 lifecycle CRUD, ACL의 `control-plane` 사용자 자동 생성 로직(이 plan에서 다룸 — Initialize 메서드).

---

## File Structure

```
.
├── config/
│   └── headscale.yaml                      # NEW Headscale config (compose + tests에서 마운트)
├── docker-compose.yml                      # MODIFY: headscale 서비스 추가
├── Makefile                                # MODIFY: headscale-init 타겟 추가
├── cmd/
│   └── control-plane/
│       └── main.go                         # MODIFY: Headscale client + policy 와이어링
├── internal/
│   ├── headscale/
│   │   ├── client.go                       # NEW HTTP client (User/Policy API)
│   │   ├── client_test.go                  # NEW testcontainer 통합 테스트
│   │   ├── acl.go                          # NEW pure ACL JSON 생성기
│   │   ├── acl_test.go                     # NEW 단위 테스트
│   │   └── testhelpers_test.go             # NEW Headscale 컨테이너 헬퍼
│   ├── policy/
│   │   ├── policy.go                       # NEW DB ↔ Headscale 동기화 orchestrator
│   │   └── policy_test.go                  # NEW 통합 테스트
│   └── users/
│       ├── service.go                      # MODIFY: HardDelete 추가
│       └── handlers.go                     # MODIFY: Policy 인터페이스 + signup 후 OnUserCreated
└── docs/superpowers/plans/
    └── 2026-05-10-headscale-integration.md (this file)
```

---

## 사전 준비 — Headscale 0.23.0 API 메모

Plan 전체에서 쓰는 Headscale API 표면을 기록해 둔다 (작업 중 참조).

| Method | Path | Body | 응답 |
|---|---|---|---|
| POST | `/api/v1/user` | `{"name": "paul"}` | `{"user": {"id":"1","name":"paul","createdAt":"..."}}` |
| GET | `/api/v1/user` | — | `{"users": [{"id":"1","name":"paul",...}, ...]}` |
| DELETE | `/api/v1/user/{name}` | — | `{}` |
| POST | `/api/v1/policy` | `{"policy": "<HuJSON 문자열>"}` | `{"policy":"...","updatedAt":"..."}` |
| GET | `/api/v1/policy` | — | `{"policy":"...","updatedAt":"..."}` |

모든 호출은 `Authorization: Bearer <api-key>` 필수. API key는 `headscale apikeys create --expiration 365d` 로 생성, 출력의 마지막 줄이 토큰.

---

### Task 1: Headscale 사이드카 + config 추가

**Files:**
- Create: `config/headscale.yaml`
- Modify: `docker-compose.yml`
- Modify: `Makefile`

- [ ] **Step 1: Headscale config 파일 작성**

`config/headscale.yaml`:
```yaml
server_url: http://headscale:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
private_key_path: /var/lib/headscale/noise_private.key
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: false
  urls:
    - https://controlplane.tailscale.com/derpmap/default
  auto_update_enabled: true
  update_frequency: 24h
disable_check_updates: true
ephemeral_node_inactivity_timeout: 30m
database:
  type: sqlite3
  sqlite:
    path: /var/lib/headscale/db.sqlite
log:
  level: info
  format: text
dns:
  magic_dns: true
  base_domain: flex
  nameservers:
    global:
      - 1.1.1.1
      - 8.8.8.8
policy:
  mode: db
acme_url: ""
acme_email: ""
unix_socket: /var/run/headscale/headscale.sock
unix_socket_permission: "0770"
```

NOTE: `policy.mode: db`가 핵심 — API로 ACL push 가능하게. `derp.server.enabled: false`로 자체 DERP 운영 비활성, 공개 DERP 사용. ip_prefixes는 표준 tailnet 대역.

- [ ] **Step 2: docker-compose.yml에 headscale 서비스 추가**

기존 `docker-compose.yml`을 다음으로 수정 (postgres 서비스 그대로 유지하고 headscale + 볼륨 추가):

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: flex
      POSTGRES_PASSWORD: flex
      POSTGRES_DB: flex
    ports:
      - "5432:5432"
    volumes:
      - flex_pg_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U flex -d flex"]
      interval: 2s
      timeout: 2s
      retries: 10

  headscale:
    image: headscale/headscale:0.23.0
    command: ["headscale", "serve"]
    volumes:
      - ./config/headscale.yaml:/etc/headscale/config.yaml:ro
      - flex_headscale_data:/var/lib/headscale
      - flex_headscale_run:/var/run/headscale
    ports:
      - "8088:8080"
    depends_on:
      postgres:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "wget", "-q", "-O-", "http://localhost:8080/health"]
      interval: 3s
      timeout: 2s
      retries: 10

volumes:
  flex_pg_data: {}
  flex_headscale_data: {}
  flex_headscale_run: {}
```

NOTE: 호스트 포트 8088 → 컨테이너 8080 매핑. 이유: 컨트롤 플레인이 8080을 점유해서 호스트에서 둘이 겹치지 않게. 컨테이너 간(docker network 내부)에서는 `http://headscale:8080`로 접근.

- [ ] **Step 3: Makefile에 headscale-init 타겟 추가**

`Makefile`의 `.PHONY` 라인에 `headscale-up headscale-init` 추가하고 파일 끝에 다음 타겟 추가:

```make
headscale-up:
	docker compose up -d headscale
	@echo "waiting for headscale to be healthy..."
	@until docker compose exec -T headscale wget -q -O- http://localhost:8080/health >/dev/null 2>&1; do sleep 1; done
	@echo "headscale is up"

headscale-init:
	@echo "creating headscale 'control-plane' user (idempotent — ignore 'already exists' error)..."
	@docker compose exec -T headscale headscale users create control-plane 2>/dev/null || true
	@echo "creating API key (1y expiration); save the printed value as FLEX_HEADSCALE_API_KEY:"
	@docker compose exec -T headscale headscale apikeys create --expiration 365d
```

- [ ] **Step 4: 수동 검증**

```bash
docker compose down -v   # 클린 슬레이트
docker compose up -d postgres headscale
make headscale-up        # 헬시 대기
make headscale-init
```

Expected:
- 첫 줄: "creating headscale 'control-plane' user..." → 성공 또는 "already exists" 무시
- 마지막 줄: 64자 hex 토큰이 stdout에 출력 — 이 값을 임시로 메모(다음 단계 수동 검증에서 사용).

`docker compose exec headscale headscale users list`
Expected: control-plane 사용자가 표에 보임.

`curl -fsS http://localhost:8088/health`
Expected: 200 OK with body `pass` 또는 `{"status":"pass"}` (Headscale 버전에 따라).

- [ ] **Step 5: Commit**

```bash
git add config/headscale.yaml docker-compose.yml Makefile
git commit -m "feat(headscale): add headscale sidecar with sqlite backend"
```

---

### Task 2: Headscale 클라이언트 스켈레톤 + ListUsers (TDD 시작)

**Files:**
- Create: `internal/headscale/client.go`
- Create: `internal/headscale/client_test.go`
- Create: `internal/headscale/testhelpers_test.go`

- [ ] **Step 1: testcontainer 헬퍼 작성**

`internal/headscale/testhelpers_test.go`:
```go
package headscale_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startHeadscale boots Headscale 0.23.0 with sqlite backend, mounts the project's
// config/headscale.yaml, exec's `headscale apikeys create` to mint an API token,
// and returns (baseURL, apiKey).
func startHeadscale(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	cfgPath, err := filepath.Abs("../../config/headscale.yaml")
	require.NoError(t, err)

	req := testcontainers.ContainerRequest{
		Image:        "headscale/headscale:0.23.0",
		ExposedPorts: []string{"8080/tcp"},
		Cmd:          []string{"headscale", "serve"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      cfgPath,
				ContainerFilePath: "/etc/headscale/config.yaml",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForHTTP("/health").
			WithPort("8080/tcp").
			WithStartupTimeout(30 * time.Second),
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

	// Mint API key via exec
	_, _, err = c.Exec(ctx, []string{"headscale", "users", "create", "control-plane"})
	require.NoError(t, err)
	rc, reader, err := c.Exec(ctx, []string{"headscale", "apikeys", "create", "--expiration", "1h"})
	require.NoError(t, err)
	require.Equal(t, 0, rc)
	out := readAll(t, reader)
	apiKey := extractAPIKey(t, out)

	baseURL := "http://" + host + ":" + port.Port()
	return baseURL, apiKey
}

func readAll(t *testing.T, r interface{ Read(p []byte) (int, error) }) string {
	t.Helper()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 256)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

func extractAPIKey(t *testing.T, out string) string {
	t.Helper()
	// `headscale apikeys create` prints noise like timestamps then the key on the last non-empty line.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		// Heuristic: the API key is hex/base64-ish, length >= 40, no spaces.
		if len(l) >= 40 && !strings.ContainsAny(l, " \t") {
			return l
		}
	}
	t.Fatalf("could not extract API key from output: %q", out)
	return ""
}
```

NOTE: testcontainers-go의 `c.Exec` 반환 타입은 `(int, io.Reader, error)` — 실제 docker exec stream은 multiplexed라 일부 노이즈가 있을 수 있음. 휴리스틱으로 마지막 non-empty 라인을 키로 채택.

- [ ] **Step 2: 실패 테스트 작성**

`internal/headscale/client_test.go`:
```go
package headscale_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/headscale"
)

func TestListUsers_InitialHasControlPlane(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	users, err := c.ListUsers(context.Background())
	require.NoError(t, err)

	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	require.Contains(t, names, "control-plane",
		"testhelpers create control-plane user; should appear in ListUsers")
}
```

- [ ] **Step 3: 테스트 실행으로 실패 확인**

Run: `go test ./internal/headscale/ -run TestListUsers -v`
Expected: 컴파일 에러 (`headscale.NewClient`, `headscale.User` 미정의).

- [ ] **Step 4: 클라이언트 스켈레톤 + ListUsers 구현**

`internal/headscale/client.go`:
```go
package headscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

type listUsersResp struct {
	Users []User `json:"users"`
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out listUsersResp
	if err := c.do(ctx, http.MethodGet, "/api/v1/user", nil, &out); err != nil {
		return nil, err
	}
	return out.Users, nil
}

func (c *Client) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		body = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("headscale %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("unmarshal: %w (body=%s)", err, string(respBody))
		}
	}
	return nil
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/headscale/ -run TestListUsers -race -count=1 -v`
Expected: PASS. 처음 실행 시 Headscale 이미지 pull로 30초~1분 소요 가능.

- [ ] **Step 6: Commit**

```bash
git add internal/headscale go.mod go.sum
git commit -m "feat(headscale): http client skeleton with ListUsers"
```

---

### Task 3: Client.CreateUser / DeleteUser

**Files:**
- Modify: `internal/headscale/client.go`
- Modify: `internal/headscale/client_test.go`

- [ ] **Step 1: 실패 테스트 작성 — `client_test.go` 끝에 추가**

```go
func TestCreateAndDeleteUser(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	u, err := c.CreateUser(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, "alice", u.Name)
	require.NotEmpty(t, u.ID)

	users, err := c.ListUsers(ctx)
	require.NoError(t, err)
	names := collectNames(users)
	require.Contains(t, names, "alice")

	require.NoError(t, c.DeleteUser(ctx, "alice"))

	users, err = c.ListUsers(ctx)
	require.NoError(t, err)
	require.NotContains(t, collectNames(users), "alice")
}

func TestCreateUser_Duplicate(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	_, err := c.CreateUser(ctx, "bob")
	require.NoError(t, err)

	_, err = c.CreateUser(ctx, "bob")
	require.ErrorIs(t, err, headscale.ErrUserAlreadyExists)
}

func TestDeleteUser_NotFound(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	err := c.DeleteUser(ctx, "ghost")
	require.ErrorIs(t, err, headscale.ErrUserNotFound)
}

func collectNames(users []headscale.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Name)
	}
	return out
}
```

- [ ] **Step 2: 실행으로 실패 확인**

Run: `go test ./internal/headscale/ -run TestCreate -v`
Expected: 컴파일 에러 (`CreateUser`, `DeleteUser`, `ErrUserAlreadyExists`, `ErrUserNotFound` 미정의).

- [ ] **Step 3: 구현**

`internal/headscale/client.go`의 `User` 구조체 정의 위에 sentinel 에러 추가:
```go
import (
	// ... existing imports
	"errors"
)

var (
	ErrUserAlreadyExists = errors.New("headscale user already exists")
	ErrUserNotFound      = errors.New("headscale user not found")
)
```

같은 파일 끝에 추가:
```go
type createUserReq struct {
	Name string `json:"name"`
}

type userResp struct {
	User User `json:"user"`
}

func (c *Client) CreateUser(ctx context.Context, name string) (User, error) {
	var out userResp
	err := c.do(ctx, http.MethodPost, "/api/v1/user", createUserReq{Name: name}, &out)
	if err != nil {
		if isHeadscaleStatusError(err, http.StatusBadRequest) ||
			isHeadscaleStatusError(err, http.StatusConflict) ||
			strings.Contains(err.Error(), "already exists") {
			return User{}, ErrUserAlreadyExists
		}
		return User{}, err
	}
	return out.User, nil
}

func (c *Client) DeleteUser(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/user/"+name, nil, nil)
	if err != nil {
		if isHeadscaleStatusError(err, http.StatusNotFound) ||
			strings.Contains(err.Error(), "not found") {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

func isHeadscaleStatusError(err error, status int) bool {
	prefix := fmt.Sprintf(": %d ", status)
	return err != nil && strings.Contains(err.Error(), prefix)
}
```

NOTE: Headscale 0.23은 같은 이름 사용자를 만들면 4xx + body에 "already exists" 류 메시지를 반환. 4xx 상태 코드 + 메시지 양쪽으로 분기 — 미래 버전 변동 대비.

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/headscale/ -race -count=1 -v`
Expected: 4 tests PASS (ListUsers, CreateAndDelete, CreateDuplicate, DeleteNotFound).

- [ ] **Step 5: Commit**

```bash
git add internal/headscale/client.go internal/headscale/client_test.go
git commit -m "feat(headscale): CreateUser / DeleteUser with sentinels"
```

---

### Task 4: Client.SetPolicy / GetPolicy

**Files:**
- Modify: `internal/headscale/client.go`
- Modify: `internal/headscale/client_test.go`

- [ ] **Step 1: 실패 테스트 작성 — `client_test.go` 끝에 추가**

```go
func TestSetAndGetPolicy(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	want := `{
  "tagOwners": {
    "tag:device-paul": ["control-plane"],
    "tag:env-paul":    ["control-plane"]
  },
  "acls": [
    {"action":"accept","src":["tag:device-paul"],"dst":["tag:env-paul:22"]}
  ]
}`

	require.NoError(t, c.SetPolicy(ctx, want))

	got, err := c.GetPolicy(ctx)
	require.NoError(t, err)
	require.Contains(t, got, "tag:device-paul")
	require.Contains(t, got, "tag:env-paul:22")
}

func TestSetPolicy_Invalid(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	err := c.SetPolicy(context.Background(), `{"this is not valid hujson`)
	require.Error(t, err, "headscale should reject malformed policy")
}
```

- [ ] **Step 2: 실행으로 실패 확인**

Run: `go test ./internal/headscale/ -run TestSet -v` and `-run TestGetPolicy`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현 — `client.go` 끝에 추가**

```go
type policyReq struct {
	Policy string `json:"policy"`
}

type policyResp struct {
	Policy    string    `json:"policy"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (c *Client) SetPolicy(ctx context.Context, hujson string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/policy", policyReq{Policy: hujson}, nil)
}

func (c *Client) GetPolicy(ctx context.Context) (string, error) {
	var out policyResp
	if err := c.do(ctx, http.MethodGet, "/api/v1/policy", nil, &out); err != nil {
		return "", err
	}
	return out.Policy, nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/headscale/ -race -count=1 -v`
Expected: 모든 6 테스트 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/headscale/client.go internal/headscale/client_test.go
git commit -m "feat(headscale): SetPolicy / GetPolicy"
```

---

### Task 5: ACL 생성기 (pure unit test)

**Files:**
- Create: `internal/headscale/acl.go`
- Create: `internal/headscale/acl_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/headscale/acl_test.go`:
```go
package headscale_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/headscale"
)

func TestGenerateACL_EmptyUsers(t *testing.T) {
	out, err := headscale.GenerateACL(nil)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Empty(t, got["tagOwners"])
	require.Empty(t, got["acls"])
}

func TestGenerateACL_SingleUser(t *testing.T) {
	out, err := headscale.GenerateACL([]string{"paul"})
	require.NoError(t, err)
	require.Contains(t, out, `"tag:device-paul"`)
	require.Contains(t, out, `"tag:env-paul"`)
	require.Contains(t, out, `"control-plane"`)

	var got struct {
		TagOwners map[string][]string `json:"tagOwners"`
		ACLs      []struct {
			Action string   `json:"action"`
			Src    []string `json:"src"`
			Dst    []string `json:"dst"`
		} `json:"acls"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	require.Equal(t, []string{"control-plane"}, got.TagOwners["tag:device-paul"])
	require.Equal(t, []string{"control-plane"}, got.TagOwners["tag:env-paul"])
	require.Len(t, got.ACLs, 1)
	require.Equal(t, "accept", got.ACLs[0].Action)
	require.Equal(t, []string{"tag:device-paul"}, got.ACLs[0].Src)
	require.Equal(t, []string{"tag:env-paul:22"}, got.ACLs[0].Dst)
}

func TestGenerateACL_TwoUsersDeterministicOrder(t *testing.T) {
	a, err := headscale.GenerateACL([]string{"paul", "alice"})
	require.NoError(t, err)
	b, err := headscale.GenerateACL([]string{"alice", "paul"})
	require.NoError(t, err)
	require.Equal(t, a, b, "output must be sorted by slug to be deterministic")

	// Both users represented
	require.Contains(t, a, "tag:device-paul")
	require.Contains(t, a, "tag:device-alice")

	// alice should appear before paul (lexicographic)
	require.True(t, strings.Index(a, "tag:device-alice") < strings.Index(a, "tag:device-paul"))
}

func TestGenerateACL_RejectsBadSlug(t *testing.T) {
	_, err := headscale.GenerateACL([]string{"Paul"})  // uppercase
	require.ErrorIs(t, err, headscale.ErrInvalidSlug)

	_, err = headscale.GenerateACL([]string{"paul space"})
	require.ErrorIs(t, err, headscale.ErrInvalidSlug)
}
```

- [ ] **Step 2: 실행으로 실패 확인**

Run: `go test ./internal/headscale/ -run TestGenerateACL -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현**

`internal/headscale/acl.go`:
```go
package headscale

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

var ErrInvalidSlug = errors.New("invalid slug for ACL generation")

// slugRe must match the slug regex enforced in users package.
var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

type aclPolicy struct {
	TagOwners map[string][]string `json:"tagOwners"`
	ACLs      []aclRule           `json:"acls"`
}

type aclRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
	Dst    []string `json:"dst"`
}

// GenerateACL produces a Headscale-compatible policy JSON for the given user slugs.
// For each slug X, generates:
//   - tagOwners["tag:device-X"] = ["control-plane"]
//   - tagOwners["tag:env-X"]    = ["control-plane"]
//   - one ACL: device-X → env-X:22
// Output is deterministic (slugs sorted ascending).
func GenerateACL(slugs []string) (string, error) {
	for _, s := range slugs {
		if !slugRe.MatchString(s) {
			return "", fmt.Errorf("%w: %q", ErrInvalidSlug, s)
		}
	}
	sorted := append([]string(nil), slugs...)
	sort.Strings(sorted)

	policy := aclPolicy{
		TagOwners: make(map[string][]string, 2*len(sorted)),
		ACLs:      make([]aclRule, 0, len(sorted)),
	}
	if len(sorted) == 0 {
		// keep empty containers (not nil) so JSON shape stays {tagOwners:{}, acls:[]}
		policy.TagOwners = map[string][]string{}
	}
	for _, s := range sorted {
		policy.TagOwners["tag:device-"+s] = []string{"control-plane"}
		policy.TagOwners["tag:env-"+s] = []string{"control-plane"}
		policy.ACLs = append(policy.ACLs, aclRule{
			Action: "accept",
			Src:    []string{"tag:device-" + s},
			Dst:    []string{"tag:env-" + s + ":22"},
		})
	}

	buf, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal acl: %w", err)
	}
	return string(buf), nil
}
```

NOTE: `MarshalIndent`로 사람이 읽을 수 있는 형태. Map 키는 Go의 `encoding/json`이 알파벳 정렬해서 직렬화하므로 결정성 보장.

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/headscale/ -run TestGenerateACL -race -count=1 -v`
Expected: 4 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/headscale/acl.go internal/headscale/acl_test.go
git commit -m "feat(headscale): pure ACL JSON generator"
```

---

### Task 6: Policy.Refresh — DB → ACL → Headscale 동기화

**Files:**
- Create: `internal/policy/policy.go`
- Create: `internal/policy/policy_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/policy/policy_test.go`:
```go
package policy_test

import (
	"context"
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

// startHeadscale boots a fresh Headscale container, creates the control-plane
// user, and returns (baseURL, apiKey).
func startHeadscale(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	cfgPath, err := filepath.Abs("../../config/headscale.yaml")
	require.NoError(t, err)

	req := testcontainers.ContainerRequest{
		Image:        "headscale/headscale:0.23.0",
		ExposedPorts: []string{"8080/tcp"},
		Cmd:          []string{"headscale", "serve"},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: cfgPath, ContainerFilePath: "/etc/headscale/config.yaml", FileMode: 0o644},
		},
		WaitingFor: wait.ForHTTP("/health").WithPort("8080/tcp").WithStartupTimeout(30 * time.Second),
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

	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 256)
	for {
		n, err := reader.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	out := string(buf)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var apiKey string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if len(l) >= 40 && !strings.ContainsAny(l, " \t") {
			apiKey = l
			break
		}
	}
	require.NotEmpty(t, apiKey, "could not extract API key from %q", out)
	return baseURL, apiKey
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
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/policy/ -v`
Expected: 컴파일 에러 (`policy.New`, `users.HardDelete`, etc.).

- [ ] **Step 3: users.Service.HardDelete 추가 (policy 테스트가 의존)**

`internal/users/service.go` 끝에 추가:
```go
// HardDelete removes the user row entirely. Used for compensation when a
// post-signup hook fails, and for tests. Returns ErrNotFound if no row matched.
func (s *Service) HardDelete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: policy 패키지 구현**

`internal/policy/policy.go`:
```go
package policy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/headscale"
)

// HeadscaleClient is the subset of headscale.Client policy needs.
type HeadscaleClient interface {
	ListUsers(ctx context.Context) ([]headscale.User, error)
	CreateUser(ctx context.Context, name string) (headscale.User, error)
	DeleteUser(ctx context.Context, name string) error
	SetPolicy(ctx context.Context, hujson string) error
}

type Policy struct {
	pool *pgxpool.Pool
	hs   HeadscaleClient
}

func New(pool *pgxpool.Pool, hs HeadscaleClient) *Policy {
	return &Policy{pool: pool, hs: hs}
}

// Refresh reads all user slugs from the DB, regenerates the ACL policy, and
// pushes it to Headscale. Idempotent and safe to call repeatedly.
func (p *Policy) Refresh(ctx context.Context) error {
	slugs, err := p.allSlugs(ctx)
	if err != nil {
		return fmt.Errorf("read slugs: %w", err)
	}
	policyJSON, err := headscale.GenerateACL(slugs)
	if err != nil {
		return fmt.Errorf("generate acl: %w", err)
	}
	if err := p.hs.SetPolicy(ctx, policyJSON); err != nil {
		return fmt.Errorf("push policy: %w", err)
	}
	return nil
}

func (p *Policy) allSlugs(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT slug FROM users ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var slugs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		slugs = append(slugs, s)
	}
	return slugs, rows.Err()
}
```

NOTE: `HeadscaleClient`는 인터페이스로 — 향후 mock/test double 가능. 실제 `*headscale.Client`는 자연스럽게 만족.

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/policy/ -race -count=1 -v`
Expected: 3 tests PASS. testcontainers가 Headscale + Postgres 두 컨테이너 띄움 — 한 테스트당 30~60초 소요.

또한:
Run: `go test ./internal/users/ -race -count=1`
Expected: 기존 테스트도 PASS (HardDelete 추가로 깨진 곳 없는지 확인).

- [ ] **Step 6: Commit**

```bash
git add internal/policy internal/users/service.go
git commit -m "feat(policy): Refresh syncs DB user slugs to Headscale ACL"
```

---

### Task 7: Policy.OnUserCreated + Initialize

**Files:**
- Modify: `internal/policy/policy.go`
- Modify: `internal/policy/policy_test.go`

- [ ] **Step 1: 실패 테스트 작성 — `policy_test.go` 끝에 추가**

```go
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

	users, err := hs.ListUsers(context.Background())
	require.NoError(t, err)
	names := make([]string, 0, len(users))
	for _, u := range users {
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
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/policy/ -run TestPolicyOnUserCreated -v` and `-run TestPolicyInitialize`
Expected: 컴파일 에러 (`OnUserCreated`, `Initialize` 미정의).

- [ ] **Step 3: 구현 — `policy.go` 수정**

상단 import 블록에 `"errors"` 추가:
```go
import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/headscale"
)
```

`policy.go` 파일 끝에 추가:
```go
// Initialize ensures the 'control-plane' user exists in Headscale (the owner
// of all tags in our ACLs). Safe to call repeatedly.
func (p *Policy) Initialize(ctx context.Context) error {
	if _, err := p.hs.CreateUser(ctx, "control-plane"); err != nil &&
		!errors.Is(err, headscale.ErrUserAlreadyExists) {
		return fmt.Errorf("create control-plane user: %w", err)
	}
	return p.Refresh(ctx)
}

// OnUserCreated is called by the signup handler immediately after a user row
// is committed to the DB. It creates the matching Headscale user (idempotent)
// and refreshes the ACL policy so the user's tags are recognized.
//
// On any error, callers should compensate by deleting the DB user row.
func (p *Policy) OnUserCreated(ctx context.Context, slug string) error {
	if _, err := p.hs.CreateUser(ctx, slug); err != nil &&
		!errors.Is(err, headscale.ErrUserAlreadyExists) {
		return fmt.Errorf("create headscale user %q: %w", slug, err)
	}
	if err := p.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh acl: %w", err)
	}
	return nil
}
```

NOTE: `errors.Is`는 `%w`로 래핑된 에러 체인도 따라가므로 `headscale.ErrUserAlreadyExists`가 클라이언트 내부에서 어떻게 반환되든 매칭됨.

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/policy/ -race -count=1 -v`
Expected: 7 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/policy/policy.go internal/policy/policy_test.go
git commit -m "feat(policy): OnUserCreated + Initialize idempotent"
```

---

### Task 8: users 패키지 — Policy 인터페이스 + signup 통합

**Files:**
- Modify: `internal/users/handlers.go`
- Modify: `internal/users/handlers_test.go`

- [ ] **Step 1: 실패 테스트 작성 — `handlers_test.go` 끝에 추가**

```go
type fakePolicy struct {
	calls       []string
	failOnSlug  string
}

func (f *fakePolicy) OnUserCreated(ctx context.Context, slug string) error {
	f.calls = append(f.calls, slug)
	if slug == f.failOnSlug {
		return errFakePolicy
	}
	return nil
}

var errFakePolicy = errors.New("fake policy failure")

func TestSignupHandler_CallsPolicyOnSuccess(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	fp := &fakePolicy{}
	h := users.NewHandlers(svc, signer, pool, fp)

	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "p@example.com", "slug": "paul", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, []string{"paul"}, fp.calls)
}

func TestSignupHandler_RollsBackOnPolicyFailure(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	fp := &fakePolicy{failOnSlug: "paul"}
	h := users.NewHandlers(svc, signer, pool, fp)

	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "p@example.com", "slug": "paul", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// User should NOT be in DB (compensation)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE slug = 'paul'`).Scan(&n))
	require.Equal(t, 0, n, "rollback should remove the user row")
}
```

또한 모든 기존 `users.NewHandlers(svc, signer, pool)` 호출을 `users.NewHandlers(svc, signer, pool, users.NoOpPolicy{})`로 교체 (총 ~10개 호출). 한 번에 sed로 가능:
```bash
# 표시 — 실제로는 Edit 도구로 한 줄씩 변경
grep -n 'NewHandlers(svc, signer, pool)' internal/users/handlers_test.go
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/users/ -v`
Expected: 컴파일 에러 (`Policy` 인터페이스, `NoOpPolicy` 미정의, 그리고 새 4-arg `NewHandlers` 시그니처 미정의).

- [ ] **Step 3: 구현 — `internal/users/handlers.go` 수정**

상단 import에 `"context"` 추가 (이미 있을 수도 있음 — 중복 OK).

`Handlers` 구조체 + `NewHandlers` 시그니처를 다음으로 교체:
```go
// Policy is a hook fired after a user is successfully signed up.
// Failure indicates the post-commit synchronization failed; the signup
// handler will compensate by deleting the user row.
type Policy interface {
	OnUserCreated(ctx context.Context, slug string) error
}

// NoOpPolicy is a Policy that does nothing — useful for tests and for the
// MVP boot path where Headscale has not been wired yet.
type NoOpPolicy struct{}

func (NoOpPolicy) OnUserCreated(_ context.Context, _ string) error { return nil }

type Handlers struct {
	svc    *Service
	signer *auth.SessionSigner
	pool   *pgxpool.Pool
	policy Policy
}

func NewHandlers(svc *Service, signer *auth.SessionSigner, pool *pgxpool.Pool, policy Policy) *Handlers {
	if policy == nil {
		policy = NoOpPolicy{}
	}
	return &Handlers{svc: svc, signer: signer, pool: pool, policy: policy}
}
```

`signup` 핸들러 — `audit.Log(...)` 호출 직후, `h.issueSession(...)` 호출 직전에 다음 추가:
```go
	if err := h.policy.OnUserCreated(r.Context(), u.Slug); err != nil {
		slog.Error("policy on-user-created", "err", err, "slug", u.Slug)
		if delErr := h.svc.HardDelete(r.Context(), u.ID); delErr != nil {
			slog.Error("rollback hard-delete", "err", delErr, "user_id", u.ID)
		}
		httperr.Write(w, http.StatusInternalServerError, "registration failed")
		return
	}
```

NOTE: audit.Log가 OnUserCreated 앞에 있는 이유 — `user.signup` 이벤트는 사용자 생성 시점을 기록. 만약 OnUserCreated 실패해서 user 롤백되면 audit_log에는 user.signup이 남아 있고 user 테이블에는 없는 상태가 되는데, 감사 로그는 본질적으로 "이 시점에 이런 일이 시도됐다"를 기록하는 것이라 OK. (보강하려면 별도 audit.Log 호출로 user.signup_rolled_back 같은 이벤트도 가능 — Plan 외 후속 작업.)

- [ ] **Step 4: 기존 테스트의 NewHandlers 호출 업데이트**

`internal/users/handlers_test.go`에서 `users.NewHandlers(svc, signer, pool)` 패턴을 모두 `users.NewHandlers(svc, signer, pool, users.NoOpPolicy{})`로 변경 (Edit 도구로 한 줄씩, 또는 replace_all=true로 일괄).

또한 새 테스트에서 사용하는 `errors` import가 file에 있는지 확인. 없으면 import 추가.

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/users/ -race -count=1 -v`
Expected: 13개 기존 + 2개 신규 = 15 tests PASS.

Run: `go vet ./...` — 클린.

- [ ] **Step 6: Commit**

```bash
git add internal/users
git commit -m "feat(users): Policy hook on signup with rollback on failure"
```

---

### Task 9: cmd/control-plane/main.go 와이어링

**Files:**
- Modify: `cmd/control-plane/main.go`

- [ ] **Step 1: main.go 수정**

기존 `main.go` import 블록에 다음 추가:
```go
	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/policy"
```

`signer` 생성 직후, `usersSvc := users.NewService(pool)` 직전에 다음 블록 추가:
```go
	hsURL := os.Getenv("FLEX_HEADSCALE_URL")
	if hsURL == "" {
		hsURL = "http://localhost:8088"
	}
	hsKey := os.Getenv("FLEX_HEADSCALE_API_KEY")
	if hsKey == "" {
		slog.Error("FLEX_HEADSCALE_API_KEY is required (run `make headscale-init` to generate)")
		os.Exit(1)
	}
	hsClient := headscale.NewClient(hsURL, hsKey, 5*time.Second)
	pol := policy.New(pool, hsClient)

	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := pol.Initialize(initCtx); err != nil {
		initCancel()
		slog.Error("policy initialize", "err", err)
		os.Exit(1)
	}
	initCancel()
```

`usersH := users.NewHandlers(usersSvc, signer, pool)`를 다음으로 교체:
```go
	usersH := users.NewHandlers(usersSvc, signer, pool, pol)
```

- [ ] **Step 2: 빌드 검증**

Run: `go build ./...`
Expected: 클린.

Run: `go vet ./...` — 클린.

- [ ] **Step 3: 수동 검증**

```bash
# postgres + headscale 둘 다 켜져 있어야
docker compose up -d postgres headscale
make headscale-up
make headscale-init
# 출력의 마지막 줄이 API key — 복사
KEY="<paste-here>"

FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
FLEX_HEADSCALE_API_KEY="$KEY" \
make run &
sleep 1

# 새 사용자 가입
curl -i -X POST http://localhost:8080/v1/auth/signup \
  -H 'content-type: application/json' \
  -d '{"email":"hs@example.com","slug":"hs-test","password":"correct-horse-battery"}'
# Expected: 201

# Headscale 상태 검증
docker compose exec headscale headscale users list
# Expected: control-plane + hs-test 둘 다 보임

docker compose exec headscale headscale policy get
# Expected: hs-test 태그가 포함된 ACL JSON

pkill -f bin/control-plane
```

- [ ] **Step 4: Commit**

```bash
git add cmd/control-plane/main.go
git commit -m "feat(control-plane): wire Headscale client + policy on startup"
```

---

### Task 10: README 업데이트 + 검증 체크리스트

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README 갱신**

`README.md`의 `### 처음 한 번` 블록을 다음으로 교체:
```markdown
### 처음 한 번

    docker compose up -d postgres headscale
    make migrate-up
    make headscale-init
```

`### 빌드/실행` 블록을 다음으로 교체:
```markdown
### 빌드/실행

`make headscale-init`이 마지막에 출력한 API key를 환경변수로 주입:

    FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
    FLEX_HEADSCALE_API_KEY="<paste-key-here>" \
    make run
```

`### 주요 환경 변수` 표에 다음 행 추가:
```markdown
| `FLEX_HEADSCALE_URL` | `http://localhost:8088` | Headscale API endpoint (compose 기본값) |
| `FLEX_HEADSCALE_API_KEY` | (필수) | Headscale API 토큰, `make headscale-init`로 생성 |
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: README updated for Headscale integration"
```

---

## End-to-end 검증 체크리스트

Plan 2 구현 완료 후 다음을 모두 통과해야 합니다.

- [ ] `make test`: 33개(Plan 1) + 신규 ~15개 = 48개 이상 테스트 PASS
- [ ] `go vet ./...`: 경고 없음
- [ ] `make headscale-up && make headscale-init`로 control-plane 사용자 + API key 발급
- [ ] 다음 시나리오 모두 동작:
  ```bash
  # postgres + headscale + migrate 적용 + API key 환경 변수 셋업
  ./bin/control-plane &
  sleep 1

  # 가입 → Headscale 동기화
  curl -fsS -X POST http://localhost:8080/v1/auth/signup \
    -H 'content-type: application/json' \
    -d '{"email":"e2e@example.com","slug":"e2e-user","password":"correct-horse-battery"}'

  # Headscale에 e2e-user 사용자가 만들어졌는지
  docker compose exec headscale headscale users list | grep e2e-user

  # ACL에 e2e-user 태그가 포함됐는지
  docker compose exec headscale headscale policy get | grep "tag:device-e2e-user"
  ```
- [ ] 컨트롤 플레인 시작 시 control-plane Headscale 사용자가 자동으로 존재(Initialize 호출)
- [ ] policy 통합 테스트의 사용자 삭제 시나리오에서 ACL이 정확히 갱신됨
- [ ] 의도적으로 잘못된 API key를 주입하면 main이 명확한 에러로 종료

이 체크리스트가 통과되면 다음 plan(Plan 3: flexctl agent + Node Pairing)으로 진행할 수 있습니다.
