# flexctl control plane

자기-주권 GPU 클라우드 플랫폼 `flexctl`의 컨트롤 플레인. 설계는 `docs/superpowers/specs/2026-05-10-flexctl-platform-design.md` 참조.

## 로컬 개발

전제: Go 1.22+, Docker(Postgres와 testcontainers용).

### 처음 한 번

    docker compose up -d postgres headscale
    make migrate-up
    make headscale-init

### 빌드/실행

`make headscale-init`이 마지막에 출력한 API key를 환경변수로 주입:

    FLEX_SESSION_SECRET=dev-secret-min-32-bytes-1234567890ab \
    FLEX_HEADSCALE_API_KEY="<paste-key-here>" \
    make run

### 테스트

    make test

testcontainers가 임시 Postgres를 띄우므로 docker daemon이 필요.

### 주요 환경 변수

| 이름 | 기본 | 설명 |
|---|---|---|
| `FLEX_ADDR` | `:8080` | HTTP 리스닝 주소 |
| `FLEX_DB_DSN` | `postgres://flex:flex@localhost:5432/flex?sslmode=disable` | Postgres 연결 |
| `FLEX_SESSION_SECRET` | (필수) | HMAC 키, 32바이트 이상 |
| `FLEX_HEADSCALE_URL` | `http://localhost:8088` | Headscale API endpoint (compose 기본값) |
| `FLEX_HEADSCALE_API_KEY` | (필수) | Headscale API 토큰, `make headscale-init`로 생성 |
| `FLEX_GRPC_ADDR` | `:9090` | gRPC agent stream 리스닝 주소 |

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
| POST | `/v1/nodes/pair-token` | session | 1회용 페어링 토큰 발급 (10분 만료) |
| POST | `/v1/nodes/pair` | token | 페어링 토큰 사용 + 노드 등록 |
| GET | `/v1/image-templates` | session | 사용 가능한 dev 이미지 템플릿 목록 |
| GET | `/v1/envs` | session | 내 환경 목록 |
| POST | `/v1/envs` | session | 환경 생성 (사이드카+dev 컨테이너 묶음) |
| GET | `/v1/envs/{id}` | session | 환경 단건 조회 |
| POST | `/v1/envs/{id}/stop` | session | 환경 중지 |
| POST | `/v1/envs/{id}/start` | session | 환경 시작 |
| DELETE | `/v1/envs/{id}` | session | 환경 삭제 (볼륨 포함) |

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

## 환경 생성 + 사이드카 빌드

### 사이드카 + dev 이미지 빌드 (운영자 1회)

GPU 서버에 다음 두 이미지가 로컬에 있어야 함 (Plan 4 MVP — registry push 없음):

    make build-flexctl       # bin/flexctl 빌드 (사이드카 Dockerfile이 COPY)
    make sidecar-image       # → flex/sidecar:dev (Alpine + tailscaled + flexctl sidecar)
    make dev-image           # → flex/dev-cuda-base:dev (nvidia/cuda + sshd)

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
