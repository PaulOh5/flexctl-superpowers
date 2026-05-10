# Control Plane Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** flexctl 컨트롤 플레인의 첫 슬라이스를 만든다 — 사용자 가입/로그인/로그아웃, 세션 쿠키, SSH 공개키 등록 CRUD, 감사 로그까지. 끝나면 curl만으로 사용자 라이프사이클을 검증할 수 있다.

**Architecture:** 단일 Go HTTP 서비스. `net/http` + `chi` v5 라우터, `pgx` v5 풀, `golang-migrate`로 마이그레이션, `argon2id`로 비밀번호 해시, HMAC-signed 쿠키로 세션(서버 측 저장소 없음). 패키지 분리는 책임 기반: `internal/auth`(공통 인증 프리미티브), `internal/users`(사용자 비즈니스 로직 + 핸들러), `internal/sshkeys`(SSH 키 핸들러), `internal/db`(DB 풀). 핸들러는 서비스 함수를 직접 호출하고 별도 repository 추상화는 두지 않는다(MVP YAGNI).

**Tech Stack:** Go 1.22+, chi v5, pgx v5, golang-migrate, golang.org/x/crypto/argon2, google/uuid, testify, testcontainers-go (Postgres 통합 테스트).

**Out of scope (다음 plan으로):** Headscale, flexctl 바이너리, 노드/환경, 웹 UI, GitHub OAuth(이메일+비밀번호만 우선).

---

## File Structure

```
.
├── cmd/
│   └── control-plane/
│       └── main.go                        # 엔트리 포인트
├── internal/
│   ├── auth/
│   │   ├── password.go                    # argon2id 해시/검증
│   │   ├── password_test.go
│   │   ├── session.go                     # HMAC-signed 쿠키 인코드/디코드
│   │   ├── session_test.go
│   │   ├── middleware.go                  # 요청에서 세션 추출, ctx에 user_id 주입
│   │   └── middleware_test.go
│   ├── db/
│   │   └── db.go                          # pgxpool 초기화 + Ping 헬퍼
│   ├── users/
│   │   ├── service.go                     # 가입/로그인/조회 로직
│   │   ├── service_test.go
│   │   ├── handlers.go                    # POST /v1/auth/signup|login|logout, GET /v1/me
│   │   └── handlers_test.go
│   ├── sshkeys/
│   │   ├── service.go                     # 키 검증/CRUD
│   │   ├── service_test.go
│   │   ├── handlers.go                    # GET/POST/DELETE /v1/me/ssh-keys
│   │   └── handlers_test.go
│   ├── audit/
│   │   ├── log.go                         # audit_log 삽입 헬퍼
│   │   └── log_test.go
│   └── httperr/
│       └── httperr.go                     # JSON 에러 응답 헬퍼
├── migrations/
│   ├── 0001_users.up.sql
│   ├── 0001_users.down.sql
│   ├── 0002_ssh_keys.up.sql
│   ├── 0002_ssh_keys.down.sql
│   ├── 0003_audit_log.up.sql
│   └── 0003_audit_log.down.sql
├── docker-compose.yml                     # 로컬 dev: postgres
├── Makefile                               # 빌드/테스트/마이그레이션 타겟
├── .gitignore
├── go.mod
├── go.sum
└── README.md
```

각 파일이 한 가지 책임만 갖도록 분리. `internal/users`와 `internal/sshkeys`는 함께 변하지 않으므로 분리. `internal/auth`는 둘 다 의존하는 공통 프리미티브.

---

### Task 1: 프로젝트 부트스트랩

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`, `README.md`
- Create: `cmd/control-plane/main.go` (skeleton)

- [ ] **Step 1: Go 모듈 초기화 및 디렉터리 생성**

```bash
cd /home/paul/flexctl-superpowers
go mod init github.com/paul/flexctl
mkdir -p cmd/control-plane internal/auth internal/db internal/users internal/sshkeys internal/audit internal/httperr migrations
```

- [ ] **Step 2: `.gitignore` 작성**

`.gitignore`:
```
/bin/
*.test
*.out
.env
.env.local
.idea/
.vscode/
```

- [ ] **Step 3: 기본 의존성 추가**

```bash
go get github.com/go-chi/chi/v5@v5.1.0
go get github.com/jackc/pgx/v5@v5.7.1
go get github.com/jackc/pgx/v5/pgxpool@v5.7.1
go get github.com/golang-migrate/migrate/v4@v4.18.1
go get github.com/golang-migrate/migrate/v4/database/postgres@v4.18.1
go get github.com/golang-migrate/migrate/v4/source/file@v4.18.1
go get github.com/google/uuid@v1.6.0
go get golang.org/x/crypto@v0.27.0
go get github.com/stretchr/testify@v1.9.0
go get github.com/testcontainers/testcontainers-go@v0.34.0
go get github.com/testcontainers/testcontainers-go/modules/postgres@v0.34.0
go mod tidy
```

- [ ] **Step 4: `cmd/control-plane/main.go` skeleton 작성**

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	addr := os.Getenv("FLEX_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("control-plane listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
```

- [ ] **Step 5: `Makefile` 작성**

```make
.PHONY: build run test lint tidy migrate-up migrate-down

DB_URL ?= postgres://flex:flex@localhost:5432/flex?sslmode=disable
MIGRATE := go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate

build:
	go build -o bin/control-plane ./cmd/control-plane

run:
	go run ./cmd/control-plane

test:
	go test ./... -race -count=1

lint:
	go vet ./...

tidy:
	go mod tidy

migrate-up:
	$(MIGRATE) -path ./migrations -database "$(DB_URL)" up

migrate-down:
	$(MIGRATE) -path ./migrations -database "$(DB_URL)" down 1
```

- [ ] **Step 6: 빌드 검증**

Run: `make build`
Expected: `bin/control-plane` 바이너리 생성, 에러 없음.

- [ ] **Step 7: 실행 검증**

Run: `make run` (백그라운드), 다른 터미널에서 `curl -i http://localhost:8080/v1/health`
Expected: `HTTP/1.1 200 OK`, body `{"status":"ok"}`. Ctrl+C로 종료.

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum .gitignore Makefile cmd/ internal/
git commit -m "feat: bootstrap control-plane skeleton with chi + healthcheck"
```

---

### Task 2: Postgres 연결 풀 + 헬스체크 통합

**Files:**
- Create: `internal/db/db.go`
- Modify: `cmd/control-plane/main.go` (DB 풀 초기화 + /v1/health에서 Ping)
- Create: `docker-compose.yml`

- [ ] **Step 1: Docker compose로 로컬 Postgres 정의**

`docker-compose.yml`:
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

volumes:
  flex_pg_data: {}
```

- [ ] **Step 2: Postgres 기동**

Run: `docker compose up -d postgres`
Expected: `flexctl-superpowers-postgres-1` 컨테이너 시작, `docker compose ps`에서 healthy.

- [ ] **Step 3: `internal/db/db.go` 작성**

```go
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
```

- [ ] **Step 4: `main.go` 수정해서 DB 풀 초기화 + /v1/health에서 Ping 사용**

`cmd/control-plane/main.go` 위쪽 import에 추가:
```go
	"github.com/paul/flexctl/internal/db"
```

`main()` 함수 안 `addr` 처리 직후, `chi.NewRouter()` 직전에 추가:
```go
	dsn := os.Getenv("FLEX_DB_DSN")
	if dsn == "" {
		dsn = "postgres://flex:flex@localhost:5432/flex?sslmode=disable"
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Open(dbCtx, dsn)
	dbCancel()
	if err != nil {
		slog.Error("db open", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
```

기존 `/v1/health` 핸들러를 다음으로 교체:
```go
	r.Get("/v1/health", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 1*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"db_down"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
```

- [ ] **Step 5: 빌드 + 실행 검증**

Run: `make build && make run` (백그라운드), `curl -i http://localhost:8080/v1/health`
Expected: `200 OK`, body `{"status":"ok"}`.

`docker compose stop postgres` 후 다시 `curl http://localhost:8080/v1/health`
Expected: `503 Service Unavailable`, body `{"status":"db_down"}`.
다시 `docker compose start postgres`.

- [ ] **Step 6: Commit**

```bash
git add docker-compose.yml internal/db cmd/control-plane/main.go
git commit -m "feat(db): pgxpool init and health check"
```

---

### Task 3: 마이그레이션 시스템 + users 테이블

**Files:**
- Create: `migrations/0001_users.up.sql`, `migrations/0001_users.down.sql`

- [ ] **Step 1: users 마이그레이션 작성**

`migrations/0001_users.up.sql`:
```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email           text NOT NULL UNIQUE,
  slug            text NOT NULL UNIQUE,
  password_hash   text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX users_email_lower_idx ON users (lower(email));
```

`migrations/0001_users.down.sql`:
```sql
DROP TABLE IF EXISTS users;
```

- [ ] **Step 2: 마이그레이션 실행 검증**

Run: `make migrate-up`
Expected: `1/u users (xxx ms)` 출력.

`docker compose exec postgres psql -U flex -d flex -c '\d users'`
Expected: 테이블 컬럼 5개(id, email, slug, password_hash, created_at) 표시.

- [ ] **Step 3: 다운/업 토글 검증**

Run: `make migrate-down && make migrate-up`
Expected: 두 명령 모두 에러 없음.

- [ ] **Step 4: Commit**

```bash
git add migrations/0001_users.up.sql migrations/0001_users.down.sql
git commit -m "feat(db): users table migration"
```

---

### Task 4: 비밀번호 해시 (argon2id)

**Files:**
- Create: `internal/auth/password.go`
- Test: `internal/auth/password_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/auth/password_test.go`:
```go
package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(hash, "$argon2id$"))

	require.NoError(t, VerifyPassword(hash, "correct-horse"))
	require.ErrorIs(t, VerifyPassword(hash, "battery-staple"), ErrPasswordMismatch)
}

func TestHashIsRandomSalted(t *testing.T) {
	h1, _ := HashPassword("same-input")
	h2, _ := HashPassword("same-input")
	require.NotEqual(t, h1, h2, "salt이 다르므로 해시도 달라야 함")
}

func TestVerifyRejectsMalformed(t *testing.T) {
	require.ErrorIs(t, VerifyPassword("not-a-real-hash", "x"), ErrInvalidHashFormat)
}
```

- [ ] **Step 2: 테스트 실행으로 실패 확인**

Run: `go test ./internal/auth/ -run TestHash -v`
Expected: 컴파일 에러 (`HashPassword`, `VerifyPassword`, `ErrPasswordMismatch`, `ErrInvalidHashFormat` 미정의).

- [ ] **Step 3: 구현 작성**

`internal/auth/password.go`:
```go
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

var (
	ErrPasswordMismatch  = errors.New("password mismatch")
	ErrInvalidHashFormat = errors.New("invalid hash format")
)

const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

func HashPassword(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func VerifyPassword(encoded, plain string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return ErrInvalidHashFormat
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return ErrInvalidHashFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrInvalidHashFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrInvalidHashFormat
	}
	got := argon2.IDKey([]byte(plain), salt, time, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/auth/ -run TestHash -v && go test ./internal/auth/ -run TestVerify -v`
Expected: 모두 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/password.go internal/auth/password_test.go
git commit -m "feat(auth): argon2id password hashing"
```

---

### Task 5: HMAC-signed 세션 쿠키

**Files:**
- Create: `internal/auth/session.go`
- Test: `internal/auth/session_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/auth/session_test.go`:
```go
package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSessionRoundTrip(t *testing.T) {
	signer := NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	uid := uuid.New()

	encoded, err := signer.Encode(Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	got, err := signer.Decode(encoded)
	require.NoError(t, err)
	require.Equal(t, uid, got.UserID)
}

func TestSessionRejectsTamperedSignature(t *testing.T) {
	signer := NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	encoded, _ := signer.Encode(Session{UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)})
	tampered := encoded[:len(encoded)-1] + "X"

	_, err := signer.Decode(tampered)
	require.ErrorIs(t, err, ErrSessionInvalid)
}

func TestSessionRejectsExpired(t *testing.T) {
	signer := NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	encoded, _ := signer.Encode(Session{UserID: uuid.New(), ExpiresAt: time.Now().Add(-time.Minute)})

	_, err := signer.Decode(encoded)
	require.ErrorIs(t, err, ErrSessionExpired)
}

func TestSessionRejectsForeignSecret(t *testing.T) {
	a := NewSessionSigner([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	b := NewSessionSigner([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))

	encoded, _ := a.Encode(Session{UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)})
	_, err := b.Decode(encoded)
	require.ErrorIs(t, err, ErrSessionInvalid)
}
```

- [ ] **Step 2: 테스트 실행으로 실패 확인**

Run: `go test ./internal/auth/ -run TestSession -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 구현 작성**

`internal/auth/session.go`:
```go
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrSessionInvalid = errors.New("session invalid")
	ErrSessionExpired = errors.New("session expired")
)

type Session struct {
	UserID    uuid.UUID `json:"uid"`
	ExpiresAt time.Time `json:"exp"`
}

type SessionSigner struct {
	key []byte
}

func NewSessionSigner(key []byte) *SessionSigner {
	if len(key) < 32 {
		panic("session signer key must be at least 32 bytes")
	}
	return &SessionSigner{key: key}
}

func (s *SessionSigner) Encode(sess Session) (string, error) {
	payload, err := json.Marshal(sess)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := s.sign(body)
	return body + "." + mac, nil
}

func (s *SessionSigner) Decode(token string) (Session, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return Session{}, ErrSessionInvalid
	}
	wantMac := s.sign(parts[0])
	if !hmac.Equal([]byte(wantMac), []byte(parts[1])) {
		return Session{}, ErrSessionInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, ErrSessionInvalid
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return Session{}, ErrSessionInvalid
	}
	if time.Now().After(sess.ExpiresAt) {
		return Session{}, ErrSessionExpired
	}
	return sess, nil
}

func (s *SessionSigner) sign(body string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/auth/ -run TestSession -v`
Expected: 모두 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/session.go internal/auth/session_test.go
git commit -m "feat(auth): hmac-signed session cookies"
```

---

### Task 6: httperr 헬퍼 + 회원가입 엔드포인트

**Files:**
- Create: `internal/httperr/httperr.go`
- Create: `internal/users/service.go`, `internal/users/service_test.go`
- Create: `internal/users/handlers.go`, `internal/users/handlers_test.go`
- Modify: `cmd/control-plane/main.go` (라우트 등록 + Signer 생성)

- [ ] **Step 1: httperr 헬퍼 작성 (테스트 없이도 trivial)**

`internal/httperr/httperr.go`:
```go
package httperr

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type Body struct {
	Error string `json:"error"`
}

func Write(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(Body{Error: msg}); err != nil {
		slog.Warn("httperr write", "err", err)
	}
}
```

- [ ] **Step 2: users 서비스 가입 함수 실패 테스트 작성**

`internal/users/service_test.go`:
```go
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
```

- [ ] **Step 3: 테스트 실행으로 실패 확인**

Run: `go test ./internal/users/ -v`
Expected: 컴파일 에러 (`users.NewService`, `users.ErrEmailTaken` 등 미정의).

- [ ] **Step 4: users 서비스 구현**

`internal/users/service.go`:
```go
package users

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/auth"
)

var (
	ErrEmailTaken       = errors.New("email already registered")
	ErrSlugTaken        = errors.New("slug already taken")
	ErrInvalidEmail     = errors.New("invalid email")
	ErrInvalidSlug      = errors.New("invalid slug")
	ErrPasswordTooShort = errors.New("password too short")
	ErrNotFound         = errors.New("user not found")
	ErrBadCredentials   = errors.New("bad credentials")
)

type User struct {
	ID    uuid.UUID
	Email string
	Slug  string
}

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

const minPasswordLen = 12

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Signup(ctx context.Context, email, slug, password string) (User, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return User{}, ErrInvalidEmail
	}
	emailNorm := strings.ToLower(addr.Address)

	slug = strings.ToLower(strings.TrimSpace(slug))
	if !slugRe.MatchString(slug) {
		return User{}, ErrInvalidSlug
	}

	if len(password) < minPasswordLen {
		return User{}, ErrPasswordTooShort
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash: %w", err)
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, slug, password_hash) VALUES ($1, $2, $3) RETURNING id`,
		emailNorm, slug, hash,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err, "users_email_key") {
			return User{}, ErrEmailTaken
		}
		if isUniqueViolation(err, "users_slug_key") {
			return User{}, ErrSlugTaken
		}
		return User{}, fmt.Errorf("insert: %w", err)
	}
	return User{ID: id, Email: emailNorm, Slug: slug}, nil
}

func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return User{}, ErrBadCredentials
	}
	emailNorm := strings.ToLower(addr.Address)

	var (
		id   uuid.UUID
		slug string
		hash string
	)
	err = s.pool.QueryRow(ctx,
		`SELECT id, slug, password_hash FROM users WHERE lower(email) = $1`,
		emailNorm,
	).Scan(&id, &slug, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrBadCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("select: %w", err)
	}
	if err := auth.VerifyPassword(hash, password); err != nil {
		return User{}, ErrBadCredentials
	}
	return User{ID: id, Email: emailNorm, Slug: slug}, nil
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	u.ID = id
	err := s.pool.QueryRow(ctx,
		`SELECT email, slug FROM users WHERE id = $1`, id,
	).Scan(&u.Email, &u.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select: %w", err)
	}
	return u, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == constraint
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/users/ -v`
Expected: 모두 PASS. (Docker가 testcontainers Postgres를 띄우는 데 30초 정도 걸릴 수 있음.)

- [ ] **Step 6: 핸들러 + 라우트 작성**

`internal/users/handlers.go`:
```go
package users

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

type Handlers struct {
	svc    *Service
	signer *auth.SessionSigner
}

func NewHandlers(svc *Service, signer *auth.SessionSigner) *Handlers {
	return &Handlers{svc: svc, signer: signer}
}

func (h *Handlers) Mount(r chi.Router) {
	r.Post("/v1/auth/signup", h.signup)
}

type signupReq struct {
	Email    string `json:"email"`
	Slug     string `json:"slug"`
	Password string `json:"password"`
}

type meResp struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Slug  string `json:"slug"`
}

func (h *Handlers) signup(w http.ResponseWriter, r *http.Request) {
	var req signupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := h.svc.Signup(r.Context(), req.Email, req.Slug, req.Password)
	switch {
	case errors.Is(err, ErrEmailTaken):
		httperr.Write(w, http.StatusConflict, "email taken")
		return
	case errors.Is(err, ErrSlugTaken):
		httperr.Write(w, http.StatusConflict, "slug taken")
		return
	case errors.Is(err, ErrInvalidEmail):
		httperr.Write(w, http.StatusBadRequest, "invalid email")
		return
	case errors.Is(err, ErrInvalidSlug):
		httperr.Write(w, http.StatusBadRequest, "invalid slug")
		return
	case errors.Is(err, ErrPasswordTooShort):
		httperr.Write(w, http.StatusBadRequest, "password too short")
		return
	case err != nil:
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}

	token, err := h.signer.Encode(auth.Session{
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "session encode")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "flex_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}
```

- [ ] **Step 7: 핸들러 통합 테스트 작성**

`internal/users/handlers_test.go`:
```go
package users_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/users"
)

func TestSignupHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)

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
	require.Len(t, resp.Cookies(), 1)
	require.Equal(t, "flex_session", resp.Cookies()[0].Name)

	var got map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "paul", got["slug"])
}

func TestSignupHandler_DuplicateReturns409(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	_, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "P@example.com", "slug": "paul2", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}
```

- [ ] **Step 8: 테스트 통과 확인**

Run: `go test ./internal/users/ -v`
Expected: 모두 PASS.

- [ ] **Step 9: main.go에 라우트 등록**

`cmd/control-plane/main.go` import에 추가:
```go
	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/users"
```

세션 시크릿 로드 + 핸들러 마운트를 router 설정 직후, `r.Get("/v1/health", ...)` 다음에 추가:
```go
	secret := []byte(os.Getenv("FLEX_SESSION_SECRET"))
	if len(secret) < 32 {
		slog.Error("FLEX_SESSION_SECRET must be at least 32 bytes")
		os.Exit(1)
	}
	signer := auth.NewSessionSigner(secret)

	usersSvc := users.NewService(pool)
	usersH := users.NewHandlers(usersSvc, signer)
	usersH.Mount(r)
```

- [ ] **Step 10: Make + 수동 검증**

`make migrate-up`이 이미 됐다고 가정. 다음 실행:
```bash
FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab make run &
sleep 1
curl -i -X POST http://localhost:8080/v1/auth/signup \
  -H 'content-type: application/json' \
  -d '{"email":"p@example.com","slug":"paul","password":"correct-horse-battery"}'
```
Expected: `HTTP/1.1 201 Created`, `Set-Cookie: flex_session=...; Path=/; HttpOnly; SameSite=Lax`, body `{"id":"...","email":"p@example.com","slug":"paul"}`.

서비스 종료(`kill %1` 또는 fg+Ctrl+C).

- [ ] **Step 11: Commit**

```bash
git add internal/httperr internal/users cmd/control-plane/main.go
git commit -m "feat(users): signup endpoint with argon2id + session cookie"
```

---

### Task 7: 로그인 엔드포인트

**Files:**
- Modify: `internal/users/handlers.go` (login 추가)
- Modify: `internal/users/handlers_test.go`

- [ ] **Step 1: 실패 테스트 작성 — `internal/users/handlers_test.go` 끝에 추가**

```go
func TestLoginHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	_, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "P@example.com", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, resp.Cookies(), 1)
}

func TestLoginHandler_BadPasswordReturns401(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	_, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "p@example.com", "password": "wrong-password-here",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestLoginHandler_UnknownEmailReturns401(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"email": "nobody@example.com", "password": "correct-horse-battery",
	})
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
```

- [ ] **Step 2: 테스트 실행으로 실패 확인**

Run: `go test ./internal/users/ -run TestLoginHandler -v`
Expected: 모두 FAIL (404 Not Found).

- [ ] **Step 3: login 핸들러 구현**

`internal/users/handlers.go`의 `Mount` 함수 수정:
```go
func (h *Handlers) Mount(r chi.Router) {
	r.Post("/v1/auth/signup", h.signup)
	r.Post("/v1/auth/login", h.login)
}
```

같은 파일 끝에 추가:
```go
type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := h.svc.Authenticate(r.Context(), req.Email, req.Password)
	if errors.Is(err, ErrBadCredentials) {
		httperr.Write(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}

	token, err := h.signer.Encode(auth.Session{
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "session encode")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "flex_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/users/ -v`
Expected: 모두 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/users/handlers.go internal/users/handlers_test.go
git commit -m "feat(users): login endpoint"
```

---

### Task 8: Auth 미들웨어 + GET /v1/me

**Files:**
- Create: `internal/auth/middleware.go`, `internal/auth/middleware_test.go`
- Modify: `internal/users/handlers.go` (me 추가, 인증 필요 라우트)
- Modify: `cmd/control-plane/main.go` (인증된 라우트 그룹 마운트)

- [ ] **Step 1: 미들웨어 실패 테스트 작성**

`internal/auth/middleware_test.go`:
```go
package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/auth"
)

func TestRequireSession_OK(t *testing.T) {
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	uid := uuid.New()
	token, _ := signer.Encode(auth.Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})

	mw := auth.RequireSession(signer)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := auth.UserIDFrom(r.Context())
		require.True(t, ok)
		require.Equal(t, uid, got)
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRequireSession_NoCookie401(t *testing.T) {
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	mw := auth.RequireSession(signer)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("inner handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRequireSession_TamperedCookie401(t *testing.T) {
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	mw := auth.RequireSession(signer)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("inner handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: "garbage.value"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
```

- [ ] **Step 2: 테스트 실행으로 실패 확인**

Run: `go test ./internal/auth/ -run TestRequireSession -v`
Expected: 컴파일 에러.

- [ ] **Step 3: 미들웨어 구현**

`internal/auth/middleware.go`:
```go
package auth

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/httperr"
)

type ctxKey int

const userIDKey ctxKey = 1

const cookieName = "flex_session"

func RequireSession(signer *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(cookieName)
			if err != nil {
				httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			sess, err := signer.Decode(c.Value)
			if err != nil {
				httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, sess.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(userIDKey).(uuid.UUID)
	return v, ok
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/auth/ -v`
Expected: 모두 PASS.

- [ ] **Step 5: GET /v1/me 핸들러 추가 + 테스트**

`internal/users/handlers.go`에 추가:
```go
func (h *Handlers) MountAuthed(r chi.Router) {
	r.Get("/v1/me", h.me)
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	u, err := h.svc.ByID(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		httperr.Write(w, http.StatusUnauthorized, "user gone")
		return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meResp{ID: u.ID.String(), Email: u.Email, Slug: u.Slug})
}
```

`internal/users/handlers_test.go`에 추가:
```go
func TestMeHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	u, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: timeNowPlusHour()})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "paul", got["slug"])
}

func TestMeHandler_NoCookie401(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/me")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
```

같은 파일 위쪽 import 옆에 헬퍼 추가:
```go
func timeNowPlusHour() time.Time { return time.Now().Add(time.Hour) }
```

import에 `"time"` 추가.

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./internal/users/ -v`
Expected: 모두 PASS.

- [ ] **Step 7: main.go에 인증된 라우트 그룹 마운트**

`cmd/control-plane/main.go`에서 `usersH.Mount(r)` 직후에 추가:
```go
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		usersH.MountAuthed(r)
	})
```

- [ ] **Step 8: 수동 검증**

```bash
FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab make run &
sleep 1
curl -i -X POST http://localhost:8080/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"p@example.com","password":"correct-horse-battery"}' \
  -c /tmp/flex-cookie.txt

curl -i -b /tmp/flex-cookie.txt http://localhost:8080/v1/me
```
Expected: 첫 요청 200 + Set-Cookie. 두 번째 요청 200 + body `{"id":"...","email":"p@example.com","slug":"paul"}`.

쿠키 없이 호출:
```bash
curl -i http://localhost:8080/v1/me
```
Expected: `401 Unauthorized`, `{"error":"unauthenticated"}`.

- [ ] **Step 9: Commit**

```bash
git add internal/auth internal/users cmd/control-plane/main.go
git commit -m "feat(auth): session middleware + GET /v1/me"
```

---

### Task 9: 로그아웃 엔드포인트

**Files:**
- Modify: `internal/users/handlers.go` (logout 추가)
- Modify: `internal/users/handlers_test.go`

- [ ] **Step 1: 실패 테스트 작성 — `handlers_test.go` 끝에 추가**

```go
func TestLogoutHandler_ClearsCookie(t *testing.T) {
	pool := newTestPool(t)
	svc := users.NewService(pool)
	u, err := svc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")
	require.NoError(t, err)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	h := users.NewHandlers(svc, signer)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		h.MountAuthed(r)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: timeNowPlusHour()})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Len(t, resp.Cookies(), 1)
	c := resp.Cookies()[0]
	require.Equal(t, "flex_session", c.Name)
	require.Equal(t, "", c.Value)
	require.True(t, c.MaxAge < 0 || c.Expires.Before(time.Now()))
}
```

- [ ] **Step 2: 테스트 실행으로 실패 확인**

Run: `go test ./internal/users/ -run TestLogoutHandler -v`
Expected: 404 또는 컴파일 에러.

- [ ] **Step 3: logout 핸들러 구현**

`internal/users/handlers.go`의 `MountAuthed` 수정:
```go
func (h *Handlers) MountAuthed(r chi.Router) {
	r.Get("/v1/me", h.me)
	r.Post("/v1/auth/logout", h.logout)
}
```

같은 파일 끝에 추가:
```go
func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "flex_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/users/ -v`
Expected: 모두 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/users/handlers.go internal/users/handlers_test.go
git commit -m "feat(users): logout endpoint"
```

---

### Task 10: ssh_keys 마이그레이션 + CRUD

**Files:**
- Create: `migrations/0002_ssh_keys.up.sql`, `migrations/0002_ssh_keys.down.sql`
- Create: `internal/sshkeys/service.go`, `service_test.go`
- Create: `internal/sshkeys/handlers.go`, `handlers_test.go`
- Modify: `cmd/control-plane/main.go`

- [ ] **Step 1: 마이그레이션 작성**

`migrations/0002_ssh_keys.up.sql`:
```sql
CREATE TABLE ssh_keys (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        text NOT NULL,
  public_key  text NOT NULL,
  fingerprint text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE(user_id, fingerprint)
);

CREATE INDEX ssh_keys_user_idx ON ssh_keys (user_id);
```

`migrations/0002_ssh_keys.down.sql`:
```sql
DROP TABLE IF EXISTS ssh_keys;
```

Run: `make migrate-up`. Expected: `2/u ssh_keys (xx ms)`.

- [ ] **Step 2: 서비스 실패 테스트 작성**

`internal/sshkeys/service_test.go`:
```go
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
```

- [ ] **Step 3: 테스트 실행으로 실패 확인**

Run: `go test ./internal/sshkeys/ -v`
Expected: 컴파일 에러.

- [ ] **Step 4: 서비스 구현 (golang.org/x/crypto/ssh로 키 검증/지문 계산)**

```bash
go get golang.org/x/crypto/ssh@v0.27.0
go mod tidy
```

`internal/sshkeys/service.go`:
```go
package sshkeys

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

var (
	ErrInvalidKey = errors.New("invalid ssh public key")
	ErrDuplicate  = errors.New("duplicate ssh key")
	ErrNotFound   = errors.New("ssh key not found")
)

type Key struct {
	ID          uuid.UUID
	Name        string
	PublicKey   string
	Fingerprint string
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Add(ctx context.Context, userID uuid.UUID, name, publicKey string) (Key, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return Key{}, ErrInvalidKey
	}
	fp := ssh.FingerprintSHA256(pk)

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO ssh_keys (user_id, name, public_key, fingerprint)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, name, publicKey, fp,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Key{}, ErrDuplicate
		}
		return Key{}, fmt.Errorf("insert: %w", err)
	}
	return Key{ID: id, Name: name, PublicKey: publicKey, Fingerprint: fp}, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, public_key, fingerprint FROM ssh_keys WHERE user_id = $1 ORDER BY created_at`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.Name, &k.PublicKey, &k.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Service) Delete(ctx context.Context, userID, keyID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM ssh_keys WHERE id = $1 AND user_id = $2`,
		keyID, userID,
	)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/sshkeys/ -v`
Expected: 모두 PASS.

- [ ] **Step 6: 핸들러 구현 + 테스트**

`internal/sshkeys/handlers.go`:
```go
package sshkeys

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/auth"
	"github.com/paul/flexctl/internal/httperr"
)

type Handlers struct {
	svc *Service
}

func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

func (h *Handlers) Mount(r chi.Router) {
	r.Get("/v1/me/ssh-keys", h.list)
	r.Post("/v1/me/ssh-keys", h.add)
	r.Delete("/v1/me/ssh-keys/{id}", h.delete)
}

type addReq struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

type keyResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

func (h *Handlers) add(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var req addReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Name == "" {
		httperr.Write(w, http.StatusBadRequest, "name required")
		return
	}
	k, err := h.svc.Add(r.Context(), uid, req.Name, req.PublicKey)
	switch {
	case errors.Is(err, ErrInvalidKey):
		httperr.Write(w, http.StatusBadRequest, "invalid public key")
		return
	case errors.Is(err, ErrDuplicate):
		httperr.Write(w, http.StatusConflict, "duplicate key")
		return
	case err != nil:
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(keyResp{ID: k.ID.String(), Name: k.Name, PublicKey: k.PublicKey, Fingerprint: k.Fingerprint})
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	keys, err := h.svc.List(r.Context(), uid)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]keyResp, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyResp{ID: k.ID.String(), Name: k.Name, PublicKey: k.PublicKey, Fingerprint: k.Fingerprint})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "invalid id")
		return
	}
	err = h.svc.Delete(r.Context(), uid, id)
	if errors.Is(err, ErrNotFound) {
		httperr.Write(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

`internal/sshkeys/handlers_test.go`:
```go
package sshkeys_test

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
	"github.com/paul/flexctl/internal/sshkeys"
	"github.com/paul/flexctl/internal/users"
)

func mountAuthed(t *testing.T, signer *auth.SessionSigner, svc *sshkeys.Service) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		sshkeys.NewHandlers(svc).Mount(r)
	})
	return httptest.NewServer(r)
}

func TestAddKeyHandler(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, sshkeys.NewService(pool))
	defer srv.Close()
	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})

	body, _ := json.Marshal(map[string]string{"name": "laptop", "public_key": validKey})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/me/ssh-keys", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestAddKeyHandler_BadKey400(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, sshkeys.NewService(pool))
	defer srv.Close()
	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})

	body, _ := json.Marshal(map[string]string{"name": "x", "public_key": "garbage"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/me/ssh-keys", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestListKeysHandler_Empty(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	u, _ := usersSvc.Signup(context.Background(), "p@example.com", "paul", "correct-horse-battery")

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, sshkeys.NewService(pool))
	defer srv.Close()
	token, _ := signer.Encode(auth.Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me/ssh-keys", nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got []map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got, 0)
}

func TestDeleteKeyHandler_OtherUser404(t *testing.T) {
	pool := newTestPool(t)
	usersSvc := users.NewService(pool)
	a, _ := usersSvc.Signup(context.Background(), "a@x.com", "alice", "correct-horse-battery")
	b, _ := usersSvc.Signup(context.Background(), "b@x.com", "bob", "correct-horse-battery")

	svc := sshkeys.NewService(pool)
	k, _ := svc.Add(context.Background(), a.ID, "alice-key", validKey)

	signer := auth.NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	srv := mountAuthed(t, signer, svc)
	defer srv.Close()
	tokenB, _ := signer.Encode(auth.Session{UserID: b.ID, ExpiresAt: time.Now().Add(time.Hour)})

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/me/ssh-keys/"+k.ID.String(), nil)
	req.AddCookie(&http.Cookie{Name: "flex_session", Value: tokenB})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
```

- [ ] **Step 7: 테스트 통과 확인**

Run: `go test ./internal/sshkeys/ -v`
Expected: 모두 PASS.

- [ ] **Step 8: main.go에 라우트 등록**

`cmd/control-plane/main.go` import에 추가:
```go
	"github.com/paul/flexctl/internal/sshkeys"
```

기존 `r.Group` 블록을 다음으로 교체:
```go
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession(signer))
		usersH.MountAuthed(r)
		sshkeys.NewHandlers(sshkeys.NewService(pool)).Mount(r)
	})
```

- [ ] **Step 9: 수동 검증**

```bash
FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab make run &
sleep 1

# 로그인하여 쿠키 저장
curl -s -X POST http://localhost:8080/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"p@example.com","password":"correct-horse-battery"}' \
  -c /tmp/flex-cookie.txt >/dev/null

# 키 추가
KEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBM5dWmqyhEfP9C1ZDjmh+e9zYx7DbT6JqnNK7NqQy11 paul@laptop"
curl -s -b /tmp/flex-cookie.txt -X POST http://localhost:8080/v1/me/ssh-keys \
  -H 'content-type: application/json' \
  -d "{\"name\":\"laptop\",\"public_key\":\"$KEY\"}" | jq

# 목록 조회
curl -s -b /tmp/flex-cookie.txt http://localhost:8080/v1/me/ssh-keys | jq
```
Expected: 첫 출력 `{"id":"...","name":"laptop","public_key":"ssh-ed25519 ...","fingerprint":"SHA256:..."}` (201). 두 번째 출력 같은 키가 배열 1개로.

- [ ] **Step 10: Commit**

```bash
git add migrations/0002_ssh_keys.up.sql migrations/0002_ssh_keys.down.sql internal/sshkeys cmd/control-plane/main.go go.mod go.sum
git commit -m "feat(sshkeys): user ssh public key registry"
```

---

### Task 11: audit_log 마이그레이션 + 인증 이벤트 기록

**Files:**
- Create: `migrations/0003_audit_log.up.sql`, `migrations/0003_audit_log.down.sql`
- Create: `internal/audit/log.go`, `log_test.go`
- Modify: `internal/users/handlers.go` (signup/login/logout 후 audit)

- [ ] **Step 1: 마이그레이션 작성**

`migrations/0003_audit_log.up.sql`:
```sql
CREATE TABLE audit_log (
  id          bigserial PRIMARY KEY,
  user_id     uuid,
  action      text NOT NULL,
  target      text,
  metadata    jsonb,
  ip          inet,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_user_idx ON audit_log (user_id, created_at DESC);
CREATE INDEX audit_log_action_idx ON audit_log (action, created_at DESC);
```

`migrations/0003_audit_log.down.sql`:
```sql
DROP TABLE IF EXISTS audit_log;
```

Run: `make migrate-up`. Expected: `3/u audit_log`.

- [ ] **Step 2: 헬퍼 실패 테스트 작성**

`internal/audit/log_test.go`:
```go
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
```

- [ ] **Step 3: 테스트 실행으로 실패 확인**

Run: `go test ./internal/audit/ -v`
Expected: 컴파일 에러.

- [ ] **Step 4: 헬퍼 구현**

`internal/audit/log.go`:
```go
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	UserID   *uuid.UUID
	Action   string
	Target   string
	Metadata map[string]any
	IP       net.IP
}

func Log(ctx context.Context, pool *pgxpool.Pool, e Event) error {
	var meta []byte
	if e.Metadata != nil {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		meta = b
	}
	var ipStr *string
	if e.IP != nil {
		s := e.IP.String()
		ipStr = &s
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO audit_log (user_id, action, target, metadata, ip)
		 VALUES ($1, $2, $3, $4, $5)`,
		e.UserID, e.Action, e.Target, meta, ipStr,
	)
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/audit/ -v`
Expected: 모두 PASS.

- [ ] **Step 6: signup/login/logout에 audit 호출 추가**

`internal/users/handlers.go` 변경 — `Handlers` 구조체에 `pool` 필드 추가:

```go
type Handlers struct {
	svc    *Service
	signer *auth.SessionSigner
	pool   *pgxpool.Pool
}

func NewHandlers(svc *Service, signer *auth.SessionSigner, pool *pgxpool.Pool) *Handlers {
	return &Handlers{svc: svc, signer: signer, pool: pool}
}
```

import에 추가:
```go
	"net"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/paul/flexctl/internal/audit"
```

`signup` 핸들러의 성공 분기 (Set-Cookie 직후) 에 추가:
```go
	_ = audit.Log(r.Context(), h.pool, audit.Event{
		UserID:   &u.ID,
		Action:   "user.signup",
		Target:   u.Slug,
		Metadata: map[string]any{"email": u.Email},
		IP:       net.ParseIP(remoteIP(r)),
	})
```

`login` 핸들러의 성공 분기 (Set-Cookie 직후) 에 추가:
```go
	_ = audit.Log(r.Context(), h.pool, audit.Event{
		UserID: &u.ID,
		Action: "auth.login",
		Target: u.Slug,
		IP:     net.ParseIP(remoteIP(r)),
	})
```

`login`의 `ErrBadCredentials` 분기 (httperr.Write 직전) 에 추가:
```go
	_ = audit.Log(r.Context(), h.pool, audit.Event{
		Action:   "auth.login_failed",
		Target:   req.Email,
		IP:       net.ParseIP(remoteIP(r)),
	})
```

`logout` 핸들러 (200 응답 직전) 에 추가:
```go
	if uid, ok := auth.UserIDFrom(r.Context()); ok {
		_ = audit.Log(r.Context(), h.pool, audit.Event{
			UserID: &uid,
			Action: "auth.logout",
			IP:     net.ParseIP(remoteIP(r)),
		})
	}
```

같은 파일 끝에 헬퍼 추가:
```go
func remoteIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// chi/middleware.RealIP가 r.RemoteAddr에 이미 반영해주지만 추가 안전망
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
```

- [ ] **Step 7: 핸들러 테스트의 NewHandlers 호출 모두 갱신**

`internal/users/handlers_test.go`의 모든 `users.NewHandlers(svc, signer)` 호출을 `users.NewHandlers(svc, signer, pool)`로 교체. (테스트마다 `pool`을 이미 만들어 사용 중이므로 그대로 전달.)

- [ ] **Step 8: 테스트 통과 확인**

Run: `go test ./... -v`
Expected: 모두 PASS.

- [ ] **Step 9: main.go 갱신**

`cmd/control-plane/main.go`에서 `NewHandlers` 호출에 pool 추가:
```go
	usersH := users.NewHandlers(usersSvc, signer, pool)
```

- [ ] **Step 10: 수동 검증**

서비스 재시작 후 가입/로그인/로그아웃 한 번씩 후:
```bash
docker compose exec postgres psql -U flex -d flex -c \
  "SELECT id, action, target, metadata FROM audit_log ORDER BY id DESC LIMIT 5;"
```
Expected: `user.signup`, `auth.login`, `auth.logout` 행이 보임.

- [ ] **Step 11: Commit**

```bash
git add migrations/0003_audit_log.up.sql migrations/0003_audit_log.down.sql internal/audit internal/users cmd/control-plane/main.go
git commit -m "feat(audit): record signup/login/logout events"
```

---

### Task 12: README + dev 워크플로 문서

**Files:**
- Create: `README.md`

- [ ] **Step 1: README 작성**

`README.md`:
```markdown
# flexctl control plane

자기-주권 GPU 클라우드 플랫폼 `flexctl`의 컨트롤 플레인. 설계는 `docs/superpowers/specs/2026-05-10-flexctl-platform-design.md` 참조.

## 로컬 개발

전제: Go 1.22+, Docker(Postgres와 testcontainers용).

### 처음 한 번

    docker compose up -d postgres
    make migrate-up

### 빌드/실행

    FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab make run

### 테스트

    make test

testcontainers가 임시 Postgres를 띄우므로 docker daemon이 필요.

### 주요 환경 변수

| 이름 | 기본 | 설명 |
|---|---|---|
| `FLEX_ADDR` | `:8080` | HTTP 리스닝 주소 |
| `FLEX_DB_DSN` | `postgres://flex:flex@localhost:5432/flex?sslmode=disable` | Postgres 연결 |
| `FLEX_SESSION_SECRET` | (필수) | HMAC 키, 32바이트 이상 |

## 현재 노출된 엔드포인트

| Method | Path | 인증 | 설명 |
|---|---|---|---|
| GET | `/v1/health` | - | DB ping 포함 |
| POST | `/v1/auth/signup` | - | 가입, 세션 쿠키 발급 |
| POST | `/v1/auth/login` | - | 로그인 |
| POST | `/v1/auth/logout` | session | 로그아웃 |
| GET | `/v1/me` | session | 현재 사용자 |
| GET | `/v1/me/ssh-keys` | session | 내 SSH 키 목록 |
| POST | `/v1/me/ssh-keys` | session | SSH 키 추가 |
| DELETE | `/v1/me/ssh-keys/{id}` | session | SSH 키 삭제 |
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: README with local dev quickstart"
```

---

## End-to-end 검증 체크리스트

Plan 1 구현 완료 후 다음을 통과해야 합니다.

- [ ] `make test`: 모든 테스트 PASS
- [ ] `go vet ./...`: 경고 없음
- [ ] 다음 시나리오가 curl로 작동:
  ```bash
  # health
  curl -fsS http://localhost:8080/v1/health
  # signup
  curl -fsS -X POST http://localhost:8080/v1/auth/signup \
    -H 'content-type: application/json' \
    -d '{"email":"p@example.com","slug":"paul","password":"correct-horse-battery"}' \
    -c /tmp/c.txt
  # me
  curl -fsS -b /tmp/c.txt http://localhost:8080/v1/me
  # add ssh key
  curl -fsS -b /tmp/c.txt -X POST http://localhost:8080/v1/me/ssh-keys \
    -H 'content-type: application/json' \
    -d '{"name":"laptop","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBM5dWmqyhEfP9C1ZDjmh+e9zYx7DbT6JqnNK7NqQy11 paul@laptop"}'
  # list ssh keys
  curl -fsS -b /tmp/c.txt http://localhost:8080/v1/me/ssh-keys
  # logout
  curl -fsS -X POST -b /tmp/c.txt http://localhost:8080/v1/auth/logout
  # /v1/me는 이제 401
  curl -i -b /tmp/c.txt http://localhost:8080/v1/me
  ```
- [ ] audit_log에 `user.signup`, `auth.login`, `auth.logout` 레코드 있음

이 체크리스트가 통과되면 다음 plan(Plan 2: Headscale Integration)으로 진행할 수 있습니다.
