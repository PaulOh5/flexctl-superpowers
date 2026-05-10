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
