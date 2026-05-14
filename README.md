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
2. Sign up: `curl -X POST https://flexctl.example.com/v1/auth/signup -d '{"email":"p@x.com","slug":"paul","password":"supersecret123"}'`.
3. From the laptop: `flexctl login --control-plane https://flexctl.example.com --email p@x.com`.
4. Build images on the GPU node: `make sidecar-image && make dev-image`.
5. Create an env (via curl): `POST /v1/envs {"name":"cuda","template_id":"cuda-base","node_id":"<uuid>","gpu_request":1}`.
6. Wait for `flexctl env list` to show `running`.
7. `flexctl ssh cuda` → `nvidia-smi` should print the GPU.

### Endpoints added by Plan 5

| Method | Path | Description |
|---|---|---|
| POST | /v1/devices/pair | Register this device, get a Headscale pre-auth key |
| GET | /v1/devices | List your devices |
| DELETE | /v1/devices/{id} | Drop a device + its Headscale node |

## Web UI

Plan 6 추가: control-plane이 React SPA를 같은 8080 포트에서 서빙한다.

### 개발

```bash
make web-dev           # Vite dev server (5173), /v1 proxy → localhost:8080
# 별도 터미널에서
make build-control-plane && ./bin/control-plane
```

브라우저에서 http://localhost:5173 (HMR 활성). API는 http://localhost:8080.

### Production build

```bash
make build-control-plane    # npm run build → internal/webui/dist → go:embed
./bin/control-plane         # 단일 바이너리, http://<host>:8080 에서 SPA + API
```

### E2E (Playwright)

```bash
make e2e        # 전체: docker-compose.e2e + migrate + control-plane + playwright
make e2e-down   # 정리
```

골든 패스 1개: signup → env create → SSH 명령 노출 검증. `FLEX_E2E_AUTOACK=1`로 dispatcher가 agent 대신 즉시 status running 마킹.

### 보안 환경 변수

| 이름 | 용도 |
|---|---|
| `FLEX_ALLOWED_ORIGINS` | comma-separated 허용 Origin (production은 외부 URL 명시). 비어있으면 dev 모드(검사 skip). |
| `FLEX_E2E_AUTOACK` | `1`이면 env dispatcher가 agent stream 우회 + 즉시 status running. **production 절대 금지.** |
