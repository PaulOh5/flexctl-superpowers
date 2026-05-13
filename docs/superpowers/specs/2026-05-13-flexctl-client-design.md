# Plan 5 — flexctl client (tsnet + SSH ProxyCommand) Design

**상태:** Draft (브레인스토밍 결과)
**날짜:** 2026-05-13
**전제:** Plan 1–4가 main에 머지되어 control-plane + agent + env lifecycle(사이드카 + dev 컨테이너) 동작 중.

## 0. 목표

사용자 노트북에서 `flexctl ssh <env>` 또는 표준 `ssh dev@<env>.flex`로 GPU dev 컨테이너에 실제 SSH 접속할 수 있게 한다. 동시에 Plan 4의 사이드카 + dev 묶음이 e2e에서 실제로 동작함을 검증한다.

**범위 안:** flexctl client CLI(`login`/`logout`/`ssh`/`proxy`/`key`/`env list`), `devices` 서브시스템(테이블 + API), tsnet 임베드, `~/.ssh/config` 자동 관리, e2e SSH 검증.

**범위 밖:** env 라이프사이클 명령(start/stop/create/delete)의 CLI 표면(Web UI에서, Plan 6+), 포트 포워딩 shorthand, OAuth 로그인, 키 회전 실시간 broadcast, VS Code 자동 설치.

## 1. 아키텍처

Plan 5는 두 개의 새 컴포넌트만 추가한다.

1. **flexctl client 모드** — `flexctl login`, `flexctl ssh`, `flexctl proxy`, `flexctl key`, `flexctl env list` 서브커맨드. `tailscale.com/tsnet` 라이브러리 임베드.
2. **control-plane devices 표면** — `devices` 테이블 + `POST /v1/devices/pair` + 부수 라우트.

env 생성 시 dev 컨테이너에 SSH 공개키를 주입하는 흐름은 Plan 4에서 이미 와이어링되어 있다 (`internal/agentstream/server.go`의 `EnvsDispatcher`가 `sshkeys.Service.List` 결과를 `CreateEnv.AuthorizedKeys`로 채워서 송신). Plan 5는 클라이언트 쪽에서 키를 등록하는 흐름만 새로 만든다.

**전체 데이터 흐름:**

```
[laptop]                              [control-plane]            [Headscale]            [GPU 노드]

flexctl login email pw ────────────►  /v1/auth/login            (이미 있음)             ┌────────────────┐
                                      ╳ session cookie ◄────                            │  agent         │
                                                                                        │  + sidecar     │
flexctl login (이어서) ────────────►  /v1/keys (POST × N)        (id_*.pub 자동 업로드) │  + dev sshd    │
                                  ─►  /v1/devices/pair          ─► CreatePreAuthKey ─►  │                │
                                  ◄─ {hostname, preauthkey,                             │                │
                                      headscale_url,                                    │                │
                                      tailnet_domain}                                   │                │

  tsnet.Server(StateDir, AuthKey)
   └─ Headscale 가입 ──────────────────────────────────────────►

  ~/.ssh/config 마커 블록 삽입

ssh dev@<env-hostname>.flex
  └─ ProxyCommand: flexctl proxy %h %p
       └─ tsnet.Dial("tcp", "<env-hostname>:22") ─tailnet─► 사이드카 tailscale0 ─netns─► dev sshd
```

**flexctl client 라이프사이클:**
- `flexctl login`: 한 번 호출 → 모든 초기 셋업 idempotent.
- `flexctl ssh <env>` 또는 `ssh dev@<env>.flex`: 매번 호출 시 tsnet 인스턴스를 새로 부팅 (state 영속이라 2번째부터 1–2초). 세션 종료 시 자동 정리.
- **백그라운드 데몬 없음.**

**격리 모델:**
- Plan 2가 이미 사용자 가입 시 `tag:device-<slug>` → `tag:env-<slug>:22` ACL을 푸시한다.
- 디바이스 페어링 시 control-plane이 `tag:device-<slug>` 태그가 붙은 pre-auth key를 발급해 새 디바이스가 자동으로 같은 사용자의 env에만 도달할 수 있게 만든다.
- 디바이스 추가가 ACL을 바꾸지 않는다 (tag는 사용자 slug 기준이지 디바이스별이 아님).

## 2. 컴포넌트 표면

### 2.1 flexctl client 명령

```
flexctl login                          # all-in-one 초기 셋업
  --control-plane <url>                # https://flexctl.example.com (client.toml에 영속)
  --email <e> --password <p>           # 비어 있으면 stdin/term prompt
  --device-name <n>                    # 기본은 OS hostname (이미 등록되어 있으면 idempotent)
  --config <path>                      # 기본 ~/.config/flexctl/client.toml

flexctl logout                         # 디바이스 삭제 + state dir clean + ssh_config 블록 제거 + client.toml 삭제

flexctl ssh <env-name> [-- <ssh-args>] # env 조회 → exec ssh, ssh-args는 그대로 forward
flexctl proxy <host> <port>            # ProxyCommand 진입점. stdin/stdout ↔ tsnet.Dial 파이프

flexctl key add <path|->                # 명시적 키 등록 (login 자동 외)
flexctl key list
flexctl key rm <id>

flexctl env list                       # 자기 env 한 줄에 NAME / HOSTNAME / STATUS / NODE 표시
```

`agent`, `sidecar`, `join`은 Plan 3/4에서 이미 있음.

### 2.2 control-plane 변경

**마이그레이션 `0009_devices`:**

```sql
CREATE TABLE devices (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name            text NOT NULL,
  hostname        text NOT NULL UNIQUE,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz,
  UNIQUE(user_id, name)
);
CREATE INDEX devices_user_id_idx ON devices(user_id);
```

**새 라우트 (`internal/devices/handlers.go`):**

| Method | Path | Auth | 동작 |
|---|---|---|---|
| POST | /v1/devices/pair | session | `{name}` → 디바이스 upsert + pre-auth key 발급 |
| GET | /v1/devices | session | 자기 디바이스 목록 |
| DELETE | /v1/devices/{id} | session | Headscale 노드 삭제 + DB 삭제 |

모두 `auth.RequireSession` 미들웨어 뒤.

**`internal/devices.Service`:**

```go
type Device struct {
    ID         uuid.UUID
    UserID     uuid.UUID
    Name       string
    Hostname   string
    CreatedAt  time.Time
    LastSeenAt *time.Time
}

type PairResult struct {
    Device         Device
    PreauthKey     string
    HeadscaleURL   string
    TailnetDomain  string  // 사용자 dial 시 사용하는 suffix, 예: "flex"
}

type Service struct {
    pool  *pgxpool.Pool
    hs    HeadscaleClient
    users *users.Service
    cfg   ServiceConfig  // HeadscaleClientURL, TailnetDomain (운영 설정)
}

func (s *Service) Pair(ctx, userID uuid.UUID, requestedName string) (PairResult, error)
func (s *Service) List(ctx, userID uuid.UUID) ([]Device, error)
func (s *Service) Delete(ctx, userID, deviceID uuid.UUID) error
func (s *Service) ByID(ctx, deviceID uuid.UUID) (Device, error)
```

`Pair`의 동작 (상세):
1. 사용자 slug 조회.
2. `hostname = "<slug>-device-<requestedName>"` (소문자, hyphen만 — 사용자가 비정상 문자 넣은 경우 400).
3. `INSERT INTO devices ... ON CONFLICT (user_id, name) DO UPDATE SET last_seen_at = now() RETURNING *`.
4. `hs.CreatePreAuthKey(user=slug, ephemeral=false, reusable=false, tags=["tag:device-<slug>"], expiration=10m)`.
5. `PairResult{Device, PreauthKey, HeadscaleURL, TailnetDomain}` 반환.

같은 (user, name)을 두 번 페어링하면 디바이스 행은 그대로, **새 pre-auth key가 매번 발급된다** — state dir이 비어 있다가 다시 채워지는 시나리오(노트북 재설치) 대응. 영속 노드가 이미 등록되어 있으면 새 키는 사용되지 않은 채 10분 만료된다(낭비 무시할 수준).

### 2.3 flexctl 패키지 구조

```
internal/flexctlcli/
  login.go              # 5단계 idempotent 흐름
  logout.go
  ssh.go                # flexctl ssh — 외부 ssh exec wrapper
  proxy.go              # flexctl proxy — ProxyCommand 진입점
  key.go                # flexctl key add/list/rm
  envlist.go            # flexctl env list
  apiclient.go          # control-plane HTTP client (cookie jar, JSON 직렬화)
  sshconfig.go          # ~/.ssh/config 마커 블록 idempotent insert/remove
  clientconfig.go       # ~/.config/flexctl/client.toml read/write

internal/flextsnet/      # 새 패키지
  server.go             # tsnet.Server 래퍼 (StateDir, AuthKey, Hostname)
  dial.go               # Dial(host:port) — proxy.go가 사용

internal/devices/        # 새 패키지
  service.go
  handlers.go
  service_test.go
  handlers_test.go
```

### 2.4 의존성 추가

- `tailscale.com/tsnet` — Tailscale 모듈을 통째로 끌어온다 (go.sum 수백 줄 증가). 대안 없음.
- `golang.org/x/term` — password prompt (이미 transitively 있을 수 있음).
- `github.com/BurntSushi/toml` 또는 `github.com/pelletier/go-toml/v2` — client.toml 직렬화 (Plan 3에서 agent.toml에 어떤 라이브러리 썼는지 확인 후 같은 것 사용).

## 3. `flexctl login` 흐름 디테일

전체 흐름은 5단계, 전부 idempotent. 어느 한 단계 실패 시 그 자리에서 멈춰 재실행 시 같은 지점부터 재개.

### 3.1 단계별

```
flexctl login --control-plane https://flexctl.example.com
  [--email ... --password ...] [--device-name ...]
```

**1. Session (`POST /v1/auth/login`)**:
- email/password가 비어 있으면 stdin/term prompt (`golang.org/x/term`).
- `Set-Cookie: flex_session=...` 받아 `~/.config/flexctl/client.toml`에 저장 (파일 모드 `0600`).
- 재실행 시 기존 cookie 유효성 확인: `GET /v1/me`(없으면 `GET /v1/keys` 같은 read-only 호출로 대용) 200이면 1단계 skip.

**2. SSH 키 자동 업로드 (`POST /v1/keys` × N)**:
- 검색 경로: `~/.ssh/id_ed25519.pub`, `~/.ssh/id_rsa.pub`, `~/.ssh/id_ecdsa.pub` (있는 것만).
- 각 키 → `GET /v1/keys`로 등록 목록과 fingerprint 비교 → 미등록만 POST.
- 이름: `<oshostname>-<keytype>` (예: `paul-macbook-ed25519`).
- 파일 0개여도 진행. stderr에 "`flexctl key add <path>`로 SSH 키를 추가하세요" 힌트. (키 0개 상태에서 env를 만들면 dev 컨테이너의 `authorized_keys`가 비어 sshd 접속 불가 — 사용자는 키 추가 후 env restart로 새 키 반영. snapshot-at-create의 귀결.)

**3. 디바이스 페어링 (`POST /v1/devices/pair`)**:
- body `{"name": <device-name>}` (기본 OS hostname, lowercase + 비ASCII 문자 stripping은 클라이언트에서).
- response `{hostname, preauthkey, headscale_url, tailnet_domain}`.
- tsnet state dir에 머신키가 이미 있고 hostname이 일치하면 단계 skip(새 pre-auth key는 사용하지 않음).

**4. tsnet 가입 (`internal/flextsnet`)**:
- StateDir: `~/.config/flexctl/tsnet/` (mode `0700`).
- `tsnet.Server{Dir, Hostname, AuthKey, ControlURL, Ephemeral: false}`.
- `srv.Start()` → `srv.Up(ctx)` (max 30s).
- 성공 시 `srv.Close()` (login은 1회용; SSH 시 다시 띄움).
- 이미 등록되어 있으면 `srv.Up`이 빠르게 200 OK이라 무해.

**5. `~/.ssh/config` 마커 블록**:
- 기존 파일을 `~/.ssh/config.flexctl-bak.<unix-timestamp>`로 백업(첫 호출 시만).
- 마커 블록이 있으면 내용 비교 후 변경 있을 때만 in-place 교체.

### 3.2 `~/.config/flexctl/client.toml`

```toml
control_plane    = "https://flexctl.example.com"
session_cookie   = "flex_session=eyJh..."
device_name      = "macbook"
device_hostname  = "paul-device-macbook"
headscale_url    = "https://headscale.example.com"
tailnet_domain   = "flex"
```

파일 모드 `0600`. `session_cookie`는 secret. 디렉토리 모드 `0700`.

### 3.3 `flexctl logout` 흐름

1. `DELETE /v1/devices/{id}` → control-plane이 Headscale 노드 삭제 + DB 삭제.
2. `POST /v1/auth/logout` (Plan 2가 만들어둔 게 있으면 호출, 없으면 cookie 만료 의존).
3. `~/.config/flexctl/tsnet/` 삭제.
4. `~/.ssh/config` 마커 블록 제거 (재백업 후 in-place).
5. `~/.config/flexctl/client.toml` 삭제.

## 4. `ssh_config` / `flexctl ssh` / `flexctl proxy`

### 4.1 `~/.ssh/config` 마커 블록 형식

```
# >>> flexctl >>>
# Managed by flexctl login. Do not edit between markers; changes will be overwritten.
Host *.flex
    User dev
    ProxyCommand /usr/local/bin/flexctl proxy %h %p
    ServerAliveInterval 30
    ServerAliveCountMax 3
    ConnectTimeout 30
    StrictHostKeyChecking accept-new
    UserKnownHostsFile ~/.config/flexctl/known_hosts
# <<< flexctl <<<
```

**디자인 메모:**
- `Host *.flex` 패턴: env hostname은 control-plane이 `<slug>-<env-name>` 형태로 반환하고 사용자는 `.flex` suffix를 붙여 SSH한다(`ssh dev@paul-vllm-train.flex`). 인터넷 `.flex` TLD가 없어 OS DNS resolver와 충돌하지 않는다.
- `%h`는 `paul-vllm-train.flex` 전체. `flexctl proxy` 내부에서 `.flex` suffix 제거 후 tsnet dial.
- `flexctl` 절대 경로는 `flexctl login` 시점의 `os.Executable()`로 결정(PATH 의존 회피).
- `StrictHostKeyChecking accept-new` + 별도 `known_hosts`: env마다 호스트키가 다른데 매번 prompt 뜨면 짜증. 별도 파일에 신뢰 누적, `~/.ssh/known_hosts` 본체는 안 건드림.

### 4.2 `flexctl ssh <env-name>` 흐름

```go
// 1. control-plane에 env 조회
GET /v1/envs                          // 사용자 own env 전체
match by name == arg
→ hostname = "paul-vllm-train"
→ status == "running" 확인
   (아니면 친절 에러: "env 'vllm-train' is <status>. ...")

// 2. ssh 실행
exec.Command("ssh",
    "-o", "ProxyCommand=" + selfPath + " proxy %h %p",
    "-o", "UserKnownHostsFile=" + knownHostsPath,
    "-o", "StrictHostKeyChecking=accept-new",
    "-o", "ConnectTimeout=30",
    "dev@" + hostname + ".flex",
    extraArgs...)
// stdin/stdout/stderr 인터랙티브 패스스루
```

**핵심**: `flexctl ssh`는 `~/.ssh/config`에 의존하지 않게 모든 옵션을 직접 넘긴다. 사용자가 마커 블록을 지우거나 손상시켜도 `flexctl ssh`는 항상 동작 (마커 블록은 VS Code Remote-SSH 등 외부 도구를 위한 것).

`-- <ssh-args>`(예: `-p 2222 -L 8888:localhost:8888`)는 그대로 forward.

### 4.3 `flexctl proxy %h %p` 흐름

```go
// host = "paul-vllm-train.flex"  port = "22"
host = strings.TrimSuffix(host, ".flex")

srv := &tsnet.Server{
    Dir:        clientCfg.StateDir,
    Hostname:   clientCfg.DeviceHostname,
    ControlURL: clientCfg.HeadscaleURL,
    Logf:       discardOrStderr,  // 기본 io.Discard, FLEXCTL_DEBUG=1이면 stderr
}
defer srv.Close()

if err := srv.Start(); err != nil { ... }
lc, _ := srv.LocalClient()
if err := waitForOnline(ctx, lc, 10*time.Second); err != nil { ... }

conn, err := srv.Dial(ctx, "tcp", host+":"+port)
if err != nil { 친절 에러 }

g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { _, err := io.Copy(conn, os.Stdin);  return err })
g.Go(func() error { _, err := io.Copy(os.Stdout, conn); return err })
return g.Wait()
```

**StateDir 동시 접근**: 여러 SSH 세션이 동시에 `flexctl proxy`를 띄우면 같은 state dir을 두 tsnet 인스턴스가 쥐려고 한다. tsnet은 file lock을 가지므로 한쪽이 실패한다. 대처:
- `srv.Start()` 실패 시 최대 5초 backoff + 1회 retry.
- 그래도 실패하면 친절 에러("another flexctl proxy is initializing, retry in a few seconds").

**시작 시간**: 첫 부팅 5–10초(노드 등록), state 재사용 1–2초. `ConnectTimeout 30s`로 여유.

### 4.4 hostname 표 정리

| 위치 | 형식 | 예시 |
|---|---|---|
| DB `envs.hostname` | `<user-slug>-<env-name>` | `paul-vllm-train` |
| Headscale 등록 hostname | 같음 | `paul-vllm-train` |
| 사용자 SSH 명령 / VS Code | `<env-hostname>.flex` | `paul-vllm-train.flex` |
| `flexctl ssh` 입력 | `<env-name>` | `vllm-train` |
| `flexctl proxy` 수신 | `<env-hostname>.flex` (→ `.flex` 제거 후 dial) | `paul-vllm-train.flex` |
| DB `devices.hostname` | `<user-slug>-device-<device-name>` | `paul-device-macbook` |

`device-` 인픽스로 env hostname과 명확히 구분된다(같은 prefix에서 충돌 회피).

## 5. devices 서브시스템 상세

### 5.1 hostname 충돌 처리

- 같은 (user_id, name)은 idempotent (`Pair`가 ON CONFLICT로 처리).
- 다른 user가 같은 name → slug가 prefix라 hostname 자체가 다름. 충돌 없음.
- 같은 사용자가 두 머신에 같은 OS hostname → `devices.hostname` UNIQUE 위반 → 409 conflict. flexctl CLI는 친절 에러로 `--device-name <other>` 권장.

### 5.2 ACL 갱신 필요 여부

Plan 2의 `policy.Refresh`가 새 user 가입 시 ACL에 `tag:device-<slug>` → `tag:env-<slug>:22` 라인을 이미 추가한다. **디바이스 추가는 ACL을 바꾸지 않는다** — tag는 사용자 slug 기준이라 디바이스 개수와 무관. `devices.Pair`는 Headscale ACL을 건드릴 필요 없이 pre-auth key의 `ACLTags`로 새 디바이스에 `tag:device-<slug>`를 자동 부여하면 끝.

### 5.3 control-plane main.go 와이어링

```go
devicesSvc := devices.NewService(pool, headscaleClient, usersSvc, devices.ServiceConfig{
    HeadscaleClientURL: os.Getenv("FLEX_HEADSCALE_CLIENT_URL"),  // 사용자 노트북이 dial하는 URL (외부 노출)
    TailnetDomain:      os.Getenv("FLEX_TAILNET_DOMAIN"),        // 기본 "flex"
})
devicesH := devices.NewHandlers(devicesSvc)
r.Group(func(r chi.Router) {
    r.Use(auth.RequireSession(signer))
    devicesH.Mount(r)
})
```

`HeadscaleClientURL`은 외부에서 도달 가능한 Headscale URL이고 기존 `headscaleClient`가 쓰는 내부 URL과 다를 수 있다(에이전트/사이드카는 같은 클러스터/VPC 안이라 내부 주소를 쓸 수 있지만 사용자 노트북은 외부 주소를 써야 한다).

## 6. 테스트 / 에러 / 범위 밖

### 6.1 테스트 레이어

| 레이어 | 도구 | 케이스 |
|---|---|---|
| 단위 | `go test` table-driven | `sshconfig.go` 마커 블록 idempotent insert/remove/preserve-other-content, `clientconfig.go` toml round-trip, devices hostname 생성 규칙 |
| HTTP 단위 | `httptest` | `devices.Handlers` Pair/List/Delete + ownership 격리 + 409 conflict |
| Service 통합 | testcontainers (Postgres + Headscale) | `devices.Service.Pair`가 실제 Headscale에 pre-auth key 발급, 같은 (user, name) 두 번 호출 idempotent, Delete가 Headscale 노드 + DB row 둘 다 정리 |
| tsnet 통합 | testcontainers Headscale + 인 프로세스 tsnet 두 개 | 디바이스 A가 디바이스 B에 dial 성공, 다른 사용자의 노드에 dial 시 ACL block (timeout) |
| flexctl 명령 단위 | mock control-plane HTTP server | `flexctl login` 5단계 idempotent 재실행, `flexctl logout`이 모든 상태 청소, key add/list/rm round-trip |
| e2e (수동, GPU 머신) | 실 노드 + 사이드카 + dev 컨테이너 | `flexctl login` → `flexctl env list` → `flexctl ssh <env>` → `nvidia-smi` 통과 |

**tsnet 통합 테스트가 핵심**: testcontainers Headscale 이미지(`headscale/headscale:0.23.0`)를 띄우고 두 `tsnet.Server`를 각각 다른 hostname/pre-auth key로 띄워 `Dial` 동작 + ACL block 검증. control-plane 없이 검증 가능.

### 6.2 에러 / 실패 모드

| 시나리오 | 처리 |
|---|---|
| `flexctl login` 중 네트워크 끊김 | 단계별 idempotent → 재실행 시 같은 지점 재개. cookie 만료면 1단계부터, tsnet state 있으면 4단계 skip |
| 동시 SSH 세션 두 개 (state dir 경쟁) | `flexctl proxy`가 `Start()` 실패 시 5초 backoff + 1회 retry. 그 후 친절 에러 |
| Headscale 일시 장애 | `devices.Pair`는 5xx. `flexctl login`은 3단계에서 멈춤 → 재시도 안내. 기존 디바이스 SSH는 영향 없음 |
| pre-auth key 만료 후 첫 tsnet 시작 | `srv.Up()` 30s 타임아웃 → 친절 에러("`flexctl login`을 다시 실행하세요") |
| state dir이 다른 머신키로 손상 | `srv.Start()` 실패 → `--reset` 플래그 안내 (state dir 삭제 후 재pair) |
| `~/.ssh/config` 사용자 직접 수정해 마커 손상 | 다음 `flexctl login`이 마커 못 찾으면 신규 블록 append. 백업 파일 보존 |
| env가 stopped/error인데 `flexctl ssh` | status 확인 후 친절 에러 |
| MagicDNS resolve 실패 (사이드카 미가입) | tsnet `Dial` 에러 그대로 + "env may still be starting, wait a few seconds" 힌트 |
| 디바이스 hostname 충돌 | 409 → `flexctl login --device-name <other>` 안내 |
| flexctl 바이너리 경로 변경 | ssh_config 절대 경로가 깨짐 → 다음 `flexctl login`이 새 경로 교체 |

### 6.3 보안 (Plan 5 새 표면)

- **디바이스 페어링 토큰**: pre-auth key 10분 만료 + reusable=false + 페어링 즉시 사용. 가로채도 한 디바이스만 등록 가능.
- **session cookie**: client.toml `0600`. 메인 스펙 기존 위협 모델 그대로.
- **tsnet state dir**: 머신키 + 노드 인증 토큰. mode `0700`. 디바이스 삭제 시 Headscale 노드 무효화 → 탈취된 state로 더 못 들어옴.

### 6.4 범위 밖

- env 라이프사이클 명령(`flexctl env start/stop/create/delete`)의 CLI 표면 — Plan 6 Web UI.
- 포트 포워딩 shorthand — 사용자가 `flexctl ssh foo -- -L 8888:localhost:8888`로 그대로 가능.
- VS Code Remote-SSH 자동 설치 — README 가이드만.
- `flexctl logout`이 ACL 정리 — Plan 2 영역 (사용자 삭제에서만).
- OAuth(GitHub) — 메인 스펙은 언급하지만 Plan 5는 email+password.
- 키 회전 실시간 broadcast — snapshot-at-create(env restart 시 반영).
- Headscale HA / 자체 DERP / 자동 업데이트 — 후속.
