# Environment Lifecycle 설계 (Plan 4)

- 작성일: 2026-05-11
- 단계: 개인 MVP / 프로토타입
- 상태: 초안 — 메인 spec(`2026-05-10-flexctl-platform-design.md`)을 보완

## 1. 목적과 범위

Plan 4의 끝나는 지점: 사용자가 컨트롤 플레인에 `POST /v1/envs`로 환경 생성 요청 → agent가 자기 GPU 서버 위에 **사이드카 컨테이너(tailscaled+TUN, NET_ADMIN) + dev 컨테이너(unprivileged sshd)** 묶음을 띄움 → Headscale tailnet에 사이드카가 가입 → `EnvReady` 응답 → DB에 `status=running`. Stop/Start/Delete까지 모두 동작. Agent 재연결 시 얕은 resync로 split-brain 해소.

본 문서는 메인 spec의 §2.3 / §3.6 / §4.3-4.5 / §5 / §7을 구현 디테일 수준으로 구체화한다. 새 control-plane 엔드포인트, gRPC 메시지, DB 스키마, agent 동작, 테스트 전략까지.

**Out of scope (Plan 4.5+)**:
- 진짜 GPU 머신에서 NVIDIA Container Toolkit 통합 검증 (수동)
- Plan 5 flexctl client(노트북 tsnet)와 결합한 e2e SSH
- 깊은 reconciliation (orphan container 정리)
- 자동 retry on error
- Sidecar 단독 crash 시 묶음 재기동(stub만)
- 이미지 자동 push/registry, 멀티 아키텍처

## 2. 선결 결정 (브레인스토밍 산물)

| 영역 | 결정 |
|---|---|
| 사이드카 이미지 | 로컬 빌드 — `make sidecar-image` → `flex/sidecar:dev` |
| dev 베이스 이미지 | `nvidia/cuda:12.4.1-base-ubuntu22.04` (→ `flex/dev-cuda-base:dev`) |
| node_resync | 얕은 — agent가 running env id 목록만 전송 |
| 테스트 전략 | 하이브리드 — mock 단위 + `//go:build integration` real Docker |
| DockerClient | 저수준 Docker SDK 1:1 wrap |
| 실패 처리 | 1회 시도, 실패 시 `status=error` → delete 후 재생성 |
| GPU 할당 | `envs.gpu_request`(int 카운트), agent가 free index 자동 할당 |

## 3. 아키텍처

```
                ┌─────────── Control Plane ───────────┐
                │  POST /v1/envs  GET/POST/DELETE     │
                │  HTTP + gRPC(:9090) + Postgres      │
                │  Headscale ephemeral-key 발급       │
                └──┬──────────────────────────────┬───┘
                   │ gRPC bidi-stream             │ Headscale REST
                   │ (CreateEnv/StopEnv/...       │ POST /api/v1/preauthkey
                   │  EnvReady/EnvError/          │ (ephemeral, 24h, tag:env-X)
                   │  EnvStateSnapshot)           │
                   ▼                              │
            ┌──────────────────────────┐          │
            │  GPU 서버 (NAT 뒤)       │          │
            │  flexctl agent           │          │
            │  ├─ envlifecycle 디스패처│          │
            │  │   + GPU 할당 맵       │          │
            │  ├─ envdocker (Docker)   │          │
            │  └─ Docker daemon ────── env 묶음들 ─┼──┐
            └──────────────────────────┘          │  │
                                                  │  │ tailnet
                                                  ▼  ▼
                                        ┌────────────────────┐
                                        │  Headscale         │
                                        │  (tailnet 가입자)  │
                                        └────────────────────┘
```

### 3.1 상태 기계

```
                           ┌─────── error ────────────────────┐
                           │                                   ▲
   creating ── create_ok ──→ running ←── stop/start ── stopped
       │                       │                         │
       └─ create_fail ─────────┴─────────────────────────┴──→ error
                                                                │
            (user delete) ↓                                     │
   running/stopped/error ──── delete ──→ deleting ──→ (DB row removed)
                                              │
                                              └─ delete_fail ──→ error
```

- 모든 전이는 컨트롤 플레인 DB에 기록된다. agent는 명령 수신 + 결과 보고만.
- `error`에서 retry 불가. 사용자는 delete 후 다른 이름으로 새 env 생성.
- `deleting` 중 실패 → `error`로 복귀. 사용자는 다시 delete 시도 가능 (idempotent).

## 4. 새 컴포넌트

```
internal/
  envs/                       # NEW
    service.go                # DB CRUD + 상태 전이 검증
    service_test.go           # testcontainers Postgres
    handlers.go               # GET/POST/DELETE /v1/envs + stop/start actions
    handlers_test.go          # httptest + chi
  imagetemplates/             # NEW (read-only, seed-driven)
    service.go                # List, ByID
    service_test.go
  envdocker/                  # NEW
    client.go                 # DockerClient 인터페이스 + RealDockerClient
    client_test.go            # mock 기반 단위
    integration_test.go       # //go:build integration
    labels.go                 # GPU 인덱스 복원 from labels
  envlifecycle/               # NEW
    dispatcher.go             # CreateEnv/Stop/Start/Delete 처리 + GPUAllocator
    dispatcher_test.go        # MockDockerClient
images/
  sidecar/Dockerfile          # NEW: Alpine + tailscaled + flexctl sidecar
  dev-cuda-base/Dockerfile    # NEW: nvidia/cuda + sshd + entrypoint
  dev-cuda-base/entrypoint.sh # authorized_keys 주입 후 sshd 시작
migrations/
  0006_image_templates.{up,down}.sql
  0007_envs.{up,down}.sql
  0008_seed_templates.{up,down}.sql
```

기존 패키지 확장만:
- `agentpb`: 6개 새 메시지 추가 (proto regen + commit)
- `agentstream/server.go`: stream 루프에 env 메시지 처리 (env_ready/env_error/env_state_snapshot 수신, CreateEnv/Stop/Start/Delete 송신)
- `flexctlagent/agent.go`: `ControlMessage`를 dispatcher에 위임
- `flexctlcli`: 새 `flexctl sidecar` 서브커맨드 (사이드카 컨테이너 내부 실행용)
- `cmd/control-plane/main.go`: envs 라우트 등록 + dispatcher 와이어링

## 5. DB 스키마

### 0006_image_templates
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

### 0007_envs
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

`ON DELETE RESTRICT` for `node_id`: 노드 삭제 전에 envs 정리 강제. MVP에선 노드 삭제 흐름 자체가 없으므로 안전한 디폴트.

### 0008_seed_templates
```sql
INSERT INTO image_templates (id, display_name, description, image_ref) VALUES
  ('cuda-base',
   'CUDA Base (Ubuntu 22.04 + CUDA 12.4)',
   'Minimal NVIDIA CUDA runtime on Ubuntu. Install PyTorch/TF via pip inside the container.',
   'flex/dev-cuda-base:dev');
```

## 6. gRPC 프로토콜 확장

`proto/agent.proto`에 다음을 추가하고 regen. 기존 `oneof payload`에 신규 메시지를 추가하는 형태.

```proto
// ─── ControlMessage payloads (server → agent) 추가 ───

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
  string preauth_key     = 2;  // 매번 fresh (사이드카가 재생성되므로)
  string authorized_keys = 3;  // 갱신 가능
}

message DeleteEnv { string env_id = 1; }

// ─── AgentMessage payloads (agent → server) 추가 ───

message EnvReady {
  // Create / Start 성공 시
  string env_id              = 1;
  string sidecar_container_id = 2;
  string dev_container_id     = 3;
  repeated int32 gpu_indices  = 4;
}

message EnvStopped {
  // Stop 성공 시 (GPU 인덱스 release됨)
  string env_id = 1;
}

message EnvDeleted {
  // Delete 성공 시 (DB row 제거 트리거)
  string env_id = 1;
}

message EnvError {
  string env_id = 1;
  string stage  = 2;   // "pull"|"sidecar_start"|"sidecar_health"|"dev_start"|"stop"|"start"|"delete"|"unknown"
  string detail = 3;
}

message EnvStateSnapshot {
  // Register 직후 reconnect 시 전송 (shallow node_resync)
  repeated string running_env_ids = 1;
}
```

기존 `AgentMessage.payload`/`ControlMessage.payload` oneof에 각 메시지 추가 (필드 번호는 기존 register/heartbeat 다음).

## 7. HTTP 엔드포인트

| Method | Path | 인증 | 본문 / 응답 |
|---|---|---|---|
| GET | `/v1/image-templates` | session | `[{id, display_name, description, image_ref, ...}]` |
| GET | `/v1/envs` | session | `[{id, name, hostname, status, status_message, node_id, template_id, gpu_request, gpu_indices, created_at, ...}]` |
| POST | `/v1/envs` | session | req `{node_id, template_id, name, gpu_request?}` → 202 + env row (status=creating). `hostname`은 컨트롤 플레인이 `<user-slug>-<env-slug>`로 자동. |
| GET | `/v1/envs/{id}` | session | single env (소유권 검증) |
| POST | `/v1/envs/{id}/stop` | session | 202 (status=stopped는 EnvReady 받은 뒤가 아닌 stop 명령 dispatch 시점에 마킹) |
| POST | `/v1/envs/{id}/start` | session | 202 |
| DELETE | `/v1/envs/{id}` | session | 202 (status=deleting). DB 행 제거는 agent delete 성공 응답 후. |

소유권: 모든 mutate 엔드포인트는 `envs.owner_user_id == auth.UserIDFrom(ctx)` 검증. 미일치 시 404 (info leak 방지).

## 8. CreateEnv lifecycle 시퀀스

```
User → Control Plane → DB → Headscale → agent → Docker

POST /v1/envs {node_id, template_id, name, gpu_request:1}
  ├─ slug 검증 (name regex), node 소유권 확인, template enabled 확인
  ├─ hostname = "<user-slug>-<env-slug>"
  ├─ INSERT envs (status=creating, volume_name=flex-env-<uuid>) → returns env_id
  ├─ Headscale POST /api/v1/preauthkey
  │     {user: "<user-slug>", reusable: false, ephemeral: true,
  │      expiration: "24h", aclTags: ["tag:env-<user-slug>"]}
  │   ← {key: "ts-..."}
  └─ stream.Send(CreateEnv{env_id, image_ref(template), sidecar_image_ref="flex/sidecar:dev",
                            hostname, headscale_url, preauth_key, tags=["tag:env-<slug>"],
                            authorized_keys, gpu_request, default_cmd})

agent (envlifecycle.Dispatcher):
  ├─ allocator.Allocate(env_id, gpu_request)  → indices or ErrInsufficientGPU
  │   on err: send EnvError{stage:"unknown", detail:"insufficient GPUs"}
  ├─ docker.CreateContainer(sidecar spec)
  │     Name: flex-net-<env_id>
  │     Image: flex/sidecar:dev
  │     CapAdd: [NET_ADMIN]
  │     Devices: [/dev/net/tun]
  │     Env: FLEXCTL_HOSTNAME, FLEXCTL_AUTHKEY, FLEXCTL_HEADSCALE_URL, FLEXCTL_TAGS
  │     Labels: flexctl.env_id=<id>, flexctl.role=sidecar,
  │             flexctl.hostname=<hostname>, flexctl.headscale_url=<url>,
  │             flexctl.tags=<csv>, flexctl.image_ref=<sidecar_image_ref>
  ├─ docker.StartContainer(sidecar_id)
  ├─ Poll tailscale status (max 30s, 1s 간격) via docker.Exec
  │     on timeout: rm sidecar, release GPU, send EnvError{stage:"sidecar_health"}
  ├─ docker.CreateContainer(dev spec)
  │     Name: flex-env-<env_id>
  │     Image: <template.image_ref>
  │     NetworkMode: container:flex-net-<env_id>
  │     GPUIndices: indices
  │     VolumeMounts: [{flex-env-<env_id> → /home/dev}]
  │     Env: FLEXCTL_AUTHORIZED_KEYS=<concat user ssh keys>
  │     Labels: flexctl.env_id=<id>, flexctl.role=dev,
  │             flexctl.gpu_indices=<csv>, flexctl.image_ref=<dev_image_ref>
  └─ docker.StartContainer(dev_id)

NOTE — 라벨이 agent의 권위 소스:
agent는 DB 접근 권한이 없으므로, Start/Delete 시점에 필요한 정보를
컨테이너 라벨 + Docker inspect로 자급자족한다. CreateEnv 시 충분한 라벨을 박아두는
것이 핵심.
       on err at any docker step: rm dev + rm sidecar (best-effort), release GPU,
                                  send EnvError{stage:"dev_start"|"pull"|...}
  └─ send EnvReady{env_id, sidecar_id, dev_id, gpu_indices}

Control Plane (agentstream.Server):
  └─ on EnvReady: UPDATE envs SET status='running', sidecar_container_id=,
                                   dev_container_id=, gpu_indices=, updated_at=now()
  └─ on EnvError: UPDATE envs SET status='error', status_message=detail
```

## 9. Stop / Start / Delete 시퀀스

### Stop
```
POST /v1/envs/{id}/stop
  ├─ UPDATE envs SET status='stopping', updated_at=now()  (transient)
  └─ stream.Send(StopEnv{env_id})

agent:
  ├─ envID로 envs 정보 조회 (라벨/inspect에서 sidecar/dev container id 회수)
  ├─ docker.StopContainer(dev_id, 10s)
  ├─ docker.StopContainer(sidecar_id, 10s)   # SIGTERM → flexctl sidecar가 tailscale logout 후 종료
  ├─ allocator.Release(env_id)
  └─ stream.Send(EnvStopped{env_id})

Control Plane on EnvStopped:
  └─ UPDATE envs SET status='stopped', gpu_indices='{}', updated_at=now()
```

### Start
```
POST /v1/envs/{id}/start
  ├─ Headscale POST /api/v1/preauthkey (새 ephemeral key, 같은 tag)
  ├─ ssh_keys 테이블에서 owner의 현재 authorized_keys 모음
  ├─ UPDATE envs SET status='starting', updated_at=now()
  └─ stream.Send(StartEnv{env_id, preauth_key, authorized_keys})

agent:
  ├─ docker.ListContainers(label flexctl.env_id=<id>) — 기존 사이드카/dev 컨테이너 조회
  ├─ 기존 사이드카 컨테이너의 hostname/tags/headscale_url/image는 label과 inspect에서 회수
  ├─ gpu_request도 dev 컨테이너 label에서 회수 → allocator.Allocate(env_id, count) (재할당)
  ├─ docker.RemoveContainer(sidecar_id)              # docker start로 ENV 변경 불가, 재생성
  ├─ docker.CreateContainer(sidecar spec, 새 FLEXCTL_AUTHKEY)
  ├─ docker.StartContainer(new_sidecar_id)
  ├─ poll tailscale status (30s)
  ├─ docker.StartContainer(dev_id)                   # dev는 기존 그대로 — `container:<sidecar_name>`은 이름 기반이라 새 사이드카로 자동 재연결
  │   authorized_keys가 갱신됐다면 docker rm/create로 새 ENV 적용 필요. MVP에선 dev는 재생성.
  ├─ docker.RemoveContainer(dev_id)
  ├─ docker.CreateContainer(dev spec, 새 FLEXCTL_AUTHORIZED_KEYS)
  ├─ docker.StartContainer(new_dev_id)
  └─ stream.Send(EnvReady{env_id, new_sidecar_id, new_dev_id, gpu_indices})

Control Plane on EnvReady:
  └─ UPDATE envs SET status='running', sidecar_container_id=..., dev_container_id=...,
                    gpu_indices=..., updated_at=now()
```

NOTE: StartEnv 메시지가 작은 이유 — agent가 라벨 + inspect로 envs 정보 자급자족. CreateEnv 시 모든 정보를 라벨로 잘 박아두는 게 중요. 갱신 가능한 필드(`authorized_keys`)만 메시지에 명시.

### Delete
```
DELETE /v1/envs/{id}
  ├─ UPDATE envs SET status='deleting'
  └─ stream.Send(DeleteEnv{env_id})

agent:
  ├─ docker.ListContainers(label flexctl.env_id=<id>) — 모든 관련 컨테이너 수집
  ├─ docker.StopContainer(dev_id) (있으면, 무시 가능 에러)
  ├─ docker.StopContainer(sidecar_id) (있으면)
  ├─ docker.RemoveContainer(dev_id, force=true)
  ├─ docker.RemoveContainer(sidecar_id, force=true)
  ├─ docker.RemoveVolume(flex-env-<env_id>)
  ├─ allocator.Release(env_id)
  └─ stream.Send(EnvDeleted{env_id})

Control Plane on EnvDeleted:
  └─ DELETE FROM envs WHERE id=$1
```

## 10. GPU 할당 (agent 측)

```go
// internal/envlifecycle/allocator.go (혹은 dispatcher.go 내)
type GPUAllocator struct {
    mu        sync.Mutex
    total     []int                       // gpuinfo.NvidiaDetector가 부팅 시 채움
    allocated map[uuid.UUID][]int         // env_id → indices
}

func (a *GPUAllocator) Allocate(envID uuid.UUID, count int) ([]int, error)
func (a *GPUAllocator) Release(envID uuid.UUID)
func (a *GPUAllocator) RestoreFromDocker(ctx context.Context, c DockerClient) error
```

`Allocate`:
- count = 0 → return `[]int{}`, nil (GPU 없이 dev 컨테이너 시작 가능)
- count > 0 → free 인덱스 collect → 부족하면 `ErrInsufficientGPU{requested, available}` 반환

`RestoreFromDocker`: agent 부팅 시 1회 호출. `docker ps --filter label=flexctl.role=dev` → 각 컨테이너의 `flexctl.gpu_indices` 라벨 파싱 → `allocated` 맵 채움. env_id도 라벨에서 회수.

agent restart 시 `gpu_indices`는 컨트롤 플레인 DB에도 있으나 agent는 라벨 기반 복원으로 자급자족 (resync 메시지 단순 유지).

## 11. DockerClient 인터페이스

```go
// internal/envdocker/client.go
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
    NetworkMode  string         // "" | "container:flex-net-<id>"
    CapAdd       []string       // ["NET_ADMIN"]
    Devices      []string       // ["/dev/net/tun"]
    GPUIndices   []int          // empty → no --gpus
    VolumeMounts []VolumeMount
}

type VolumeMount struct {
    Source string  // named volume
    Target string  // "/home/dev"
}

type ContainerInfo struct {
    ID     string
    Name   string
    State  string             // "created"|"running"|"exited"|...
    Labels map[string]string
}

type DockerClient interface {
    CreateContainer(ctx context.Context, spec ContainerSpec) (id string, err error)
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

`Real` 구현: `github.com/docker/docker/client.Client` 래퍼. `Mock` 구현: hand-rolled (testify/mock 옵션 — 단위 테스트가 호출 순서 + 인자 검증).

GPU 옵션은 `ContainerSpec.GPUIndices`를 Docker의 `DeviceRequests` 로 변환:
```go
if len(spec.GPUIndices) > 0 {
    var ids []string
    for _, i := range spec.GPUIndices { ids = append(ids, strconv.Itoa(i)) }
    hostConfig.Resources.DeviceRequests = []container.DeviceRequest{{
        Driver:       "nvidia",
        Capabilities: [][]string{{"gpu"}},
        DeviceIDs:    ids,
    }}
}
```

## 12. 에러 lifecycle (실패 단계별)

| 실패 단계 | agent 동작 | EnvError.stage | 컨트롤 플레인 |
|---|---|---|---|
| GPU 부족 | (컨테이너 미시작) | `"unknown"` + detail | `status=error`, message=detail |
| Sidecar `docker pull` | (best-effort) | `"pull"` | `status=error` |
| Sidecar start | rm 사이드카 | `"sidecar_start"` | `status=error` |
| Sidecar health timeout 30s | rm 사이드카 | `"sidecar_health"` | `status=error` |
| Dev `docker pull` | rm 사이드카 | `"pull"` | `status=error` |
| Dev start | rm dev + rm 사이드카 | `"dev_start"` | `status=error` |
| Stop 중 stop 실패 | log + GPU release | `"stop"` | `status=error` |
| Start 중 실패 | (위 create와 동일 경로) | 위와 동일 | `status=error` |
| Delete 중 실패 (rm/volume) | log + GPU release | `"delete"` | `status=error`, manual cleanup 가이드 |

**Sidecar 단독 crash 중 running** (in-flight):
- agent의 Docker events watcher (Plan 4 범위 — 단순 1-시나리오):
  - dev 컨테이너 exit detected → 같은 env_id의 사이드카 살아 있으면 dev만 restart
  - 사이드카 exit detected → Plan 4에선 log만, dev는 그대로 둠 (Plan 4.5에서 묶음 재기동)

## 13. Agent 재연결 시 resync

```
agent process restart 후:
  1. allocator.RestoreFromDocker() — 라벨에서 GPU 할당 복원
  2. flexctlagent.runOnce()가 Register 전송
  3. (NEW) 직후 EnvStateSnapshot 전송:
     - docker.ListContainers(label flexctl.role=dev, state=running)
     - 각 컨테이너의 flexctl.env_id 라벨 수집
     - running_env_ids = 그 목록
  4. 이후 Heartbeat 루프

control plane on EnvStateSnapshot:
  - DB SELECT id FROM envs WHERE node_id=$1 AND status='running'
  - lost = DB 집합 - agent_report 집합
  - for each id in lost: UPDATE envs SET status='error', status_message='lost during reconnect'
  - 반대 방향(agent_report - DB): Plan 4에서 무시. Plan 4.5에서 orphan cleanup.
```

## 14. 사이드카 Dockerfile 및 `flexctl sidecar` 서브커맨드

```dockerfile
# images/sidecar/Dockerfile
FROM alpine:3.20
RUN apk add --no-cache iptables ip6tables ca-certificates
ARG TS_VERSION=1.76.6
RUN wget -q https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_amd64.tgz \
 && tar xzf tailscale_${TS_VERSION}_amd64.tgz \
 && mv tailscale_${TS_VERSION}_amd64/tailscaled /usr/local/bin/ \
 && mv tailscale_${TS_VERSION}_amd64/tailscale  /usr/local/bin/ \
 && rm -rf tailscale_${TS_VERSION}_amd64*
COPY bin/flexctl /usr/local/bin/flexctl
ENTRYPOINT ["/usr/local/bin/flexctl", "sidecar"]
```

`flexctl sidecar` 모드 동작:
1. ENV 읽기: `FLEXCTL_HOSTNAME`, `FLEXCTL_AUTHKEY`, `FLEXCTL_HEADSCALE_URL`, `FLEXCTL_TAGS` (쉼표 구분)
2. 디렉터리 준비: `mkdir -p /var/lib/tailscale /var/run/tailscale`
3. `tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/var/run/tailscale/tailscaled.sock --tun=tailscale0` 백그라운드 goroutine으로 실행
4. `tailscaled` 준비 대기 (소켓 ready) → `tailscale up --login-server=<FLEXCTL_HEADSCALE_URL> --authkey=<FLEXCTL_AUTHKEY> --hostname=<FLEXCTL_HOSTNAME> --advertise-tags=<FLEXCTL_TAGS> --accept-dns=false`
5. SIGTERM 받으면 `tailscale logout` → tailscaled 종료 → exit 0

## 15. dev base Dockerfile

```dockerfile
# images/dev-cuda-base/Dockerfile
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

`entrypoint.sh`:
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

## 16. 테스트 전략

| 레이어 | 위치 | 방식 |
|---|---|---|
| envs DB service | `internal/envs/service_test.go` | testcontainers Postgres, 상태 전이 검증 |
| envs HTTP handlers | `internal/envs/handlers_test.go` | httptest + chi, 소유권 검증 포함 |
| imagetemplates | `internal/imagetemplates/service_test.go` | testcontainers Postgres |
| envdocker (mock) | `internal/envdocker/client_test.go` | hand-rolled MockDockerClient — 인자 + 호출 순서 검증 |
| envdocker (real) | `internal/envdocker/integration_test.go` | `//go:build integration` — 진짜 Docker daemon에 alpine 컨테이너 create/start/stop/rm |
| envlifecycle dispatcher | `internal/envlifecycle/dispatcher_test.go` | MockDockerClient + flow별 검증 (create happy, sidecar timeout, dev start fail, stop, delete, GPU 부족) |
| agentstream env 명령 | `internal/agentstream/server_test.go` 확장 | 진짜 gRPC + 진짜 nodes DB + MockDispatcher (envlifecycle 모킹) |
| 전체 e2e (Plan 5 없이) | `internal/envs/e2e_test.go` | testcontainers Postgres + Headscale + 진짜 agent goroutine + MockDockerClient → POST /v1/envs → EnvReady → DB status=running 검증 |

수동 검증 (Plan 4 끝나면): 진짜 GPU 머신 + 진짜 Docker daemon + 진짜 Headscale 사이드카에서 `make sidecar-image && curl POST /v1/envs && docker ps`로 두 컨테이너 + tailnet 가입 확인. Plan 5에서 진짜 SSH까지.

## 17. 미해결 / 후속 (Plan 4.5+)

- 깊은 reconciliation: agent가 docker ps 결과로 orphan 컨테이너 정리
- error → retry 흐름 (Plan 4에선 delete-then-recreate)
- 사이드카 단독 crash 시 묶음 자동 재기동
- 다중 dev 이미지 템플릿 (PyTorch, JupyterLab, vLLM)
- 이미지 GHCR push 자동화 + 멀티 아키텍처
- env idle suspend / 사용 시간 제한
- 다중 GPU 인덱스 명시 지정 / GPU memory 단위 할당
- env 안에서 추가 포트 노출 (예: JupyterLab 8888)
