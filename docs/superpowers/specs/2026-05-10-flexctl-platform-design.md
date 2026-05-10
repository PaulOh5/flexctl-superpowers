# flexctl GPU 클라우드 플랫폼 설계

- 작성일: 2026-05-10
- 단계: 개인 MVP / 프로토타입
- 상태: 초안

## 1. 목적과 범위

개인이 보유한 리눅스 GPU 서버를 자기-주권적 클라우드 노드로 사용할 수 있게 해주는 플랫폼을 만든다. 사용자는 자기 GPU 서버에 단일 바이너리 `flexctl`을 설치해 플랫폼에 페어링하고, 웹에서 큐레이션된 템플릿으로 GPU 컨테이너 개발환경을 띄운 뒤, 자기 노트북에서 SSH(및 VS Code Remote-SSH)로 접속해 일한다. 등록되는 GPU 서버는 NAT 뒤에 있을 수 있고, 사용자 노트북 ↔ 컨테이너 통신은 셀프 호스트한 Headscale 메시 위에서 이루어진다.

본 설계는 MVP(공개 가입은 받되 운영은 1인) 범위까지 정의한다. 결제, 멀티-GPU 분배, idle suspend, HA 등은 명시적으로 후속 작업으로 둔다.

### 핵심 결정 요약

| 영역 | 결정 |
|---|---|
| 단계 | 개인 MVP, 공개 가입 허용 |
| 제어 채널 | flexctl agent → 퍼블릭 컨트롤 플레인으로 outbound gRPC bidi-stream |
| 개발환경 접속 | SSH 중심 (VS Code Remote-SSH 호환) |
| Tailscale | 셀프 호스트 Headscale, 컨테이너마다 고유 노드 |
| Tailnet 경계 | 단일 tailnet, tag 기반 ACL로 사용자 격리 |
| 이미지 카탈로그 | 운영자 큐레이션 템플릿 4–6종 |
| 스택 | Go 풀스택 (백엔드 + flexctl), 웹은 React |
| flexctl 형태 | 단일 Go 바이너리, 3가지 모드(`agent`/`init`/`client`) + tsnet 임베드 |
| 사용자 인증 | 이메일+비밀번호 + 선택적 GitHub OAuth, Tailscale 계정 불필요 |

## 2. 시스템 개요와 컴포넌트

```
                    ┌─────────────────────────────────┐
                    │   Control Plane (퍼블릭 인터넷)  │
                    │  - Web UI (React)               │
                    │  - API (Go)                     │
                    │  - Postgres                     │
                    │  - Headscale (사이드 컨테이너)  │
                    └─────────────────────────────────┘
                          ▲                ▲
                outbound  │                │ tsnet → Headscale
              gRPC stream │                │
                          │                │
        ┌─────────────────┘                └────────────────┐
        │                                                    │
┌───────────────────┐                              ┌─────────────────┐
│  GPU 서버 (NAT 뒤)│                              │ 사용자 노트북   │
│  flexctl agent    │                              │ flexctl client  │
│  ├─ Docker daemon │                              │  + tsnet daemon │
│  └─ 컨테이너들 ──→ tsnet → Headscale ←──── tsnet─┘                │
│       (각자 노드) │                              │ ProxyCommand SSH│
└───────────────────┘                              └─────────────────┘
```

### 2.1 Control Plane

- Go HTTP/gRPC API 서버, React SPA, Postgres.
- Headscale은 같은 호스트(또는 같은 docker compose) 안에 동거하지만 컨트롤 플레인과는 **HTTP API로만** 통신한다(느슨 결합).
- 책임: 사용자 가입/인증, 노드 페어링 토큰 발급, agent 인증과 명령 발행, Headscale에 ACL/pre-auth key 발급 위임, 메타데이터 영속화.

### 2.2 flexctl agent (사용자 GPU 서버)

- `systemd` 유닛으로 상시 실행.
- 컨트롤 플레인에 outbound gRPC bidi-stream으로 영구 연결, jittered exponential backoff(상한 60s)로 재연결.
- Docker SDK로 컨테이너 라이프사이클 관리, NVIDIA Container Toolkit으로 GPU 패스스루.
- 노드 헬스/리소스(GPU 모델, VRAM, 가용 여부) 주기 보고.
- agent 자체는 tailnet에 가입하지 않는다. tailnet 멤버는 컨테이너와 사용자 디바이스만.

### 2.3 flexctl init (컨테이너 내부)

- 모든 큐레이션 베이스 이미지에 작은 Go 바이너리로 포함.
- 컨테이너 부팅 시 ENV에서 pre-auth key, 호스트네임, 태그를 읽어 `tsnet`으로 Headscale 가입.
- ENV로 주입된 사용자 SSH 공개키들을 `/home/dev/.ssh/authorized_keys`에 기록.
- 별도 `sshd`를 띄워 `tsnet`이 노출한 가상 인터페이스에 바인드 → VS Code Remote-SSH/sftp/scp/포트포워딩 등 OpenSSH 전체 호환.
- 컨테이너 종료 시 tsnet logout → ephemeral 노드는 Headscale에서 자동 정리.

### 2.4 flexctl client (사용자 노트북)

- 단일 바이너리, Linux/macOS/Windows.
- `flexctl login` → 브라우저 OAuth → access/refresh 토큰 저장.
- `flexctl up` → 백그라운드 데몬이 tsnet으로 Headscale 가입.
- 명령:
  - `flexctl ls` — 내 환경 목록
  - `flexctl up <template> --node <node>` — 환경 생성
  - `flexctl ssh <env-name>` — 직접 SSH
  - `flexctl proxy <host> <port>` — `~/.ssh/config`의 ProxyCommand로 사용
- 최초 login 시 `~/.ssh/config`에 `Host *.flex` 블록 자동 갱신.

## 3. 네트워크 / Tailscale 토폴로지

### 3.1 식별/네이밍

```
Tailnet 도메인:    flex
사용자 디바이스:   <user-slug>-<device>.flex      예) paul-laptop.flex
GPU 노드 (호스트): tailnet 미가입
컨테이너 환경:     <user-slug>-<env-slug>.flex    예) paul-vllm-train.flex
```

### 3.2 태그 설계

Headscale/Tailscale ACL은 노드의 단일 태그로 src/dst 매칭을 한다. 사용자 격리를 단순한 ACL 라인으로 표현하기 위해 **사용자별-역할별 태그**를 사용한다.

- `tag:device-<user-slug>` — 사용자 노트북(예: `tag:device-paul`)
- `tag:env-<user-slug>` — 사용자 환경 컨테이너(예: `tag:env-paul`)

GPU 호스트는 tailnet 미가입(별도 태그 불필요). 컨트롤 플레인이 모든 태그의 owner.

### 3.3 ACL

핵심 규칙은 **"`tag:device-X` 노드만 `tag:env-X` 노드의 22번 포트에 접근"**. 컨트롤 플레인이 사용자 가입/삭제 시 사용자별 라인을 동적 생성해 ACL JSON을 재생성하고 Headscale에 push.

```json
{
  "tagOwners": {
    "tag:device-paul": ["control-plane@flex"],
    "tag:env-paul":    ["control-plane@flex"],
    "tag:device-jane": ["control-plane@flex"],
    "tag:env-jane":    ["control-plane@flex"]
  },
  "acls": [
    { "action": "accept",
      "src":    ["tag:device-paul"],
      "dst":    ["tag:env-paul:22"] },
    { "action": "accept",
      "src":    ["tag:device-jane"],
      "dst":    ["tag:env-jane:22"] }
  ]
}
```

기본 정책은 deny(생략). ACL push 실패 시 환경 생성은 fail-closed.

### 3.4 Pre-auth key 전략

| 대상 | 키 종류 | 만료 | 재사용 |
|---|---|---|---|
| 컨테이너 환경 | ephemeral, reusable=false | 24h | 1회 |
| 사용자 노트북 | non-ephemeral, reusable=false | 90일 | 디바이스 1개 |

flexctl client는 만료 7일 전부터 백그라운드로 키 갱신.

### 3.5 DERP

MVP는 Tailscale 공식 공개 DERP를 그대로 사용. Headscale 설정에 공식 derpmap URL 지정. 자체 DERP 운영은 후속.

### 3.6 컨테이너 안 tsnet 동작

- `flexctl init`이 PID 1(`tini`로 reaping 위임 후 fork) 또는 그 자식.
- `tsnet.Server{Hostname, AuthKey, ControlURL}` 시작.
- 가상 인터페이스 위에 별도 `sshd`를 바인드(호환성 우선).
- 사용자 SSH 공개키는 ENV 주입 → 디스크 `authorized_keys`로 반영.

## 4. 핵심 사용자 흐름

### 4.1 가입 + 노트북 페어링

1. 사용자가 웹에서 가입(이메일+비밀번호 또는 GitHub OAuth).
2. flexctl 다운로드 후 `flexctl login` → 브라우저 OAuth → access/refresh 토큰 + Headscale pre-auth key 수령.
3. `flexctl up` → tsnet 데몬 시작 → Headscale 가입 → `~/.ssh/config` 갱신.

### 4.2 GPU 서버 등록

1. 웹에서 "노드 추가" → 1회용 페어링 토큰(10분 만료) 표시.
2. 사용자 GPU 서버에서 `sudo flexctl join FX-XXXX-YYYY` 실행.
3. agent가 `POST /v1/nodes/pair`로 토큰 + GPU 정보 송신, 검증 후 `node_token`(장기) 수령.
4. `/etc/flexctl/agent.toml` 저장, systemd 활성화, gRPC stream 연결.

### 4.3 환경 생성

1. 사용자가 웹에서 템플릿 선택 + 환경 이름 + 노드 선택.
2. 컨트롤 플레인이 `envs` 행 생성(creating), Headscale에 ephemeral pre-auth key 발급, agent stream에 `create_env{...}` 푸시.
3. agent가 `docker create + start`(–gpus all, ENV로 pre-auth key/hostname/tags/authorized_keys 주입).
4. 컨테이너의 `flexctl init`이 tsnet으로 Headscale 가입, `sshd` 시작, agent에 `env_ready` 보고.
5. 컨트롤 플레인이 `envs.status=running` 갱신, 웹 UI는 SSH 접속 안내(`ssh dev@paul-vllm-train.flex`).

### 4.4 SSH 접속

```bash
# flexctl 직접
flexctl ssh vllm-train

# 일반 ssh / VS Code Remote-SSH
ssh dev@paul-vllm-train.flex
# → ~/.ssh/config의 ProxyCommand=flexctl proxy 매칭
# → flexctl proxy가 tsnet 안에서 dial, stdin/stdout pipe
```

VS Code Remote-SSH는 표준 OpenSSH 클라이언트를 사용하므로 그대로 동작.

### 4.5 Stop / Start / Delete

- **Stop**: agent에 `stop_env` → `docker stop` → ephemeral 노드 자동 제거. 볼륨 유지.
- **Start**: 새 pre-auth key 발급 + `start_env` → 같은 hostname으로 재가입.
- **Delete**: `docker rm -v`(볼륨까지) + Headscale 잔여 정리 + DB 삭제.

### 4.6 agent 재연결

stream 끊김 시 jittered backoff. 재연결 직후 agent가 `node_resync{running_envs[]}`를 첫 메시지로 보내 양쪽 상태 일치(split-brain 방지).

## 5. 데이터 모델 (Postgres)

```sql
users (
  id              uuid pk,
  email           text unique not null,
  slug            text unique not null,
  password_hash   text,
  github_id       bigint unique,
  created_at      timestamptz
)

ssh_keys (
  id              uuid pk,
  user_id         uuid fk,
  name            text,
  public_key      text not null,
  created_at      timestamptz
)

devices (
  id                uuid pk,
  user_id           uuid fk,
  name              text,
  headscale_node_id text,
  last_seen_at      timestamptz,
  created_at        timestamptz
)

nodes (
  id              uuid pk,
  owner_user_id   uuid fk,
  name            text,
  agent_version   text,
  gpu_info        jsonb,
  status          text,
  node_token_hash text,
  last_seen_at    timestamptz,
  created_at      timestamptz,
  unique(owner_user_id, name)
)

pair_tokens (
  token_hash      text pk,
  user_id         uuid fk,
  expires_at      timestamptz,
  used_at         timestamptz
)

image_templates (
  id              text pk,
  display_name    text,
  description     text,
  image_ref       text not null,
  default_cmd     text[],
  enabled         bool default true
)

envs (
  id              uuid pk,
  owner_user_id   uuid fk,
  node_id         uuid fk,
  template_id     text fk,
  name            text,
  hostname        text,
  status          text,
  container_id    text,
  gpu_request     int default 1,
  volume_name     text,
  created_at      timestamptz,
  unique(owner_user_id, name)
)

audit_log (
  id          bigserial pk,
  user_id     uuid,
  action      text,
  target      text,
  metadata    jsonb,
  ip          inet,
  created_at  timestamptz
)
```

영속성: 컨테이너의 `/home/dev`를 named docker volume(`flex-env-<env_id>`)에 마운트. stop/start 후에도 보존, delete 시에만 제거.

## 6. 보안 / 인증

### 6.1 인증 매트릭스

| 주체 | 인증 수단 | 만료 |
|---|---|---|
| 사용자 (웹) | session cookie (HttpOnly, SameSite=Lax, Secure) | 30일 sliding |
| 사용자 (flexctl client) | OAuth bearer + refresh | access 1h / refresh 30일 |
| flexctl agent | node_token (장기, hash 저장) | 회전 가능, MVP 무기한 |
| 컨트롤 플레인 → Headscale | Headscale API key | 정적 |

### 6.2 위협 모델 핵심

1. **타 사용자 환경 침입** — Headscale ACL이 `tag:device-X` → `tag:env-X:22`만 허용. ACL 변경 권한은 컨트롤 플레인만.
2. **타 사용자 노드에 컨테이너 띄우기** — API가 `nodes.owner_user_id = current_user`를 항상 검증. agent도 stream 명령에서 owner 일치를 재검증(defense-in-depth).
3. **노드 페어링 토큰 탈취** — 1회용 + 10분 만료 + HTTPS only.
4. **컨테이너 → 호스트 escape** — docker default. MVP는 self-pwn(자기 노드 자기 사용)이라 외부 위협 아님. gVisor/Kata는 후속.
5. **컨트롤 플레인 침해 영향** — argon2id 패스워드 해시, AES-GCM secret encrypt-at-rest, KMS 키 환경변수.
6. **flexctl 바이너리 변조** — 릴리즈 SHA256 + Sigstore 서명(MVP 후).

### 6.3 사용자 격리 검증

통합 테스트에서 "사용자 A의 디바이스로 사용자 B의 환경 SSH 시도가 ACL에 의해 거부되는지" 매 빌드 검증.

## 7. 에러 / 실패 모드

| 시나리오 | 처리 |
|---|---|
| agent ↔ 컨트롤 플레인 stream 끊김 | jittered exp backoff 재연결, 재연결 시 `node_resync`로 양쪽 상태 일치 |
| 컨테이너 생성 중 agent 크래시 | 다음 stream 연결 시 컨트롤 플레인이 status=creating env 재조회 → 컨테이너 존재 시 running, 없으면 error |
| Headscale 일시 장애 | 환경 생성은 5xx로 실패. 기존 환경 SSH는 영향 없음(직결 후엔 Headscale 비참여) |
| pre-auth key 만료 후 재시작 | flexctl init 등록 실패 → agent 보고 → 새 키 발급 후 재시도 |
| GPU 서버 power-off | agent offline 마킹. Docker restart 정책이 노드 복귀 시 자동 복구 |
| ACL push 실패 | 환경 생성 fail-closed, 부분 생성 컨테이너 정리 |
| 사용자 SSH 키 회전 | 모든 running env에 `update_authorized_keys` broadcast |

## 8. 테스트 전략

| 레이어 | 도구 | 핵심 케이스 |
|---|---|---|
| 단위 | `go test` table-driven | ACL 생성기, 토큰 검증, slug 변환 |
| 통합 | testcontainers (Postgres, Headscale) | 가입→페어링→환경 생성→SSH 접속 e2e, A→B 격리 |
| flexctl agent | mock 컨트롤 플레인 + 실제 docker | 재연결, 명령 idempotency, snapshot resync |
| Tailscale 경로 | `tsnet` 인스턴스 + 임베디드 Headscale | 이름 해석, dial, ACL 차단 |
| UI | Playwright | 가입→환경 생성→SSH URL 표시 골든 패스 |

GPU/CUDA 동작 검증은 dev 노드 한 대에서 수동 smoke. CI는 NVIDIA 부분 mock.

## 9. 미해결 / 후속

- 멀티 GPU 분배 (현재 컨테이너당 1 GPU, 노드 GPU 모두 점유)
- idle suspend / 사용 시간 제한
- 노드 공유 (타 사용자에게 빌려주기)
- 컨테이너 escape 강화 (gVisor / Kata)
- Headscale HA / 자체 DERP
- flexctl 자동 업데이트 채널
- 결제/사용량 측정
- 다중 NVIDIA 외 GPU(AMD ROCm, Intel) 지원
