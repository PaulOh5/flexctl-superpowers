# Plan 6 — Web UI Design

**상태:** Draft (브레인스토밍 결과)
**날짜:** 2026-05-14
**전제:** Plan 1–5가 main에 머지되어 control-plane HTTP/gRPC + flexctl agent + flexctl client + 사이드카+dev 컨테이너 lifecycle까지 실제 GPU 머신 e2e로 검증됨.

## 0. 목표

메인 스펙 §4.4의 **골든 패스(가입 → 환경 생성/관리 → SSH URL 안내)**를 브라우저 한 곳에서 끝낼 수 있게 한다. control-plane HTTP API는 그대로 재사용하고, 새 라우트는 `GET /v1/nodes` 하나만 추가. Playwright로 골든 패스 1개를 자동 검증.

**범위 안:** 7 페이지 React SPA(signup/login/dashboard/envs-new/envs-detail/keys/devices) + Vite + Tailwind + shadcn/ui + go:embed로 control-plane 단일 바이너리 + Playwright e2e 1 시나리오.

**범위 밖:** GitHub OAuth, 비밀번호 재설정, 사용량 차트, 다크 모드, 모바일 최적화, 실시간 push 알림, /nodes 페이지, /settings 페이지, 노드 공유.

## 1. 아키텍처

**스택**:
- 빌드: Vite + React 18 + TypeScript
- 라우팅: React Router v6 (browser history)
- 데이터: TanStack Query (server state 캐싱 + 자동 refetch — env status `creating → running` 폴링)
- UI: Tailwind CSS + shadcn/ui (CLI vendor 컴포넌트)
- 호스팅: 빌드된 dist를 `go:embed`로 control-plane Go 바이너리에 포함. 같은 8080 포트에서 SPA + API.

**디렉토리 레이아웃**:

```
web/
├── src/
│   ├── main.tsx                # Vite entry
│   ├── App.tsx                 # Router + QueryProvider
│   ├── lib/
│   │   ├── api.ts              # control-plane fetch wrapper (credentials: 'include')
│   │   └── types.ts            # User, Env, Device, SSHKey, Node, ImageTemplate
│   ├── components/
│   │   ├── ui/                 # shadcn vendor 컴포넌트
│   │   ├── Layout.tsx          # sidebar + main 영역
│   │   ├── RequireSession.tsx  # auth guard
│   │   └── StatusBadge.tsx
│   ├── pages/
│   │   ├── Signup.tsx
│   │   ├── Login.tsx
│   │   ├── Dashboard.tsx       # env card grid + "New env" CTA
│   │   ├── EnvNew.tsx          # 폼: name, template, node, gpu_request
│   │   ├── EnvDetail.tsx       # status, hostname, SSH 명령, stop/start/delete
│   │   ├── Keys.tsx            # SSH 키 view/add/rm
│   │   └── Devices.tsx         # 디바이스 view/rm
│   └── index.css
├── e2e/
│   └── golden-path.spec.ts
├── index.html
├── vite.config.ts
├── tailwind.config.js
├── playwright.config.ts
├── package.json
└── tsconfig.json

internal/webui/
├── dist/                       # Vite output (build artifact, gitignored)
└── embed.go                    # //go:embed all:dist

internal/spa/
├── handler.go                  # fs + SPA fallback
└── handler_test.go
```

**데이터 흐름**:

```
Browser
  ┌─ /  /envs/new  /envs/:id  /keys  /devices  /login  /signup  → SPA (embedded)
  └─ /v1/*  → existing Go handlers (+ 새로 추가: GET /v1/nodes)
```

**인증**: 기존 `flex_session` HttpOnly cookie 그대로. SameSite=Lax + Origin 헤더 체크(POST/DELETE)로 CSRF 방어. 단일 도메인이라 CSRF 토큰 별도 불필요.

## 2. 페이지 / UX 흐름

### 2.1 라우트 표

| Path | 인증 | 목적 |
|---|---|---|
| `/signup` | 공개 | email + slug + password 가입 |
| `/login` | 공개 | email + password 로그인 |
| `/` | session | dashboard: env 카드 그리드 + "New env" 버튼 |
| `/envs/new` | session | 폼: name, template, node, gpu_request → POST /v1/envs |
| `/envs/:id` | session | status badge, hostname, SSH 명령 복사, stop/start/delete |
| `/keys` | session | 등록된 SSH 키 목록 + add (textarea) + rm |
| `/devices` | session | 등록된 디바이스 목록(hostname/last_seen) + rm |

(logout은 sidebar 버튼 → POST /v1/auth/logout → /login redirect.)

전역 레이아웃: 좌측 sidebar (Dashboard / Keys / Devices / Logout) + 우측 메인 영역. signup/login은 sidebar 없는 centered 레이아웃.

### 2.2 골든 패스 (Playwright e2e 검증 대상)

```
1. 새 브라우저에서 /signup
   - 폼 제출 → 200 + cookie set → / 으로 redirect
2. / (dashboard)
   - "no environments yet" empty state
   - "New env" 클릭 → /envs/new
3. /envs/new
   - template dropdown (GET /v1/image-templates, 기본 "cuda-base")
   - node dropdown (GET /v1/nodes — Plan 6에서 추가)
   - name input + gpu_request number
   - 제출 → POST /v1/envs → /envs/:id 로 redirect
4. /envs/:id
   - status badge: "creating" (yellow)
   - TanStack Query refetch interval 3s → "running" (green)이 되면 SSH 카드 노출
   - 카드에 두 명령 노출: `flexctl ssh <name>` 와 `ssh dev@<hostname>.flex` (둘 다 Copy 버튼)
5. Playwright assertion: 두 코드 블록 모두 DOM에 존재
```

이 단일 시나리오만 Playwright로 통과 검증 (메인 스펙 §8). keys/devices CRUD는 단위 테스트만.

### 2.3 status badge 색상

| status | 색상 (Tailwind) |
|---|---|
| `creating` | `bg-yellow-100 text-yellow-800` |
| `running` | `bg-green-100 text-green-800` |
| `stopped` | `bg-gray-100 text-gray-800` |
| `error` | `bg-red-100 text-red-800` + tooltip으로 `status_message` 노출 |

### 2.4 인증 가드

`<RequireSession>` 래퍼: 마운트 시 `GET /v1/me` → 401이면 `/login`으로 redirect (현재 path를 query param에 보존). 200이면 자식 렌더. session-protected 라우트는 전부 이 래퍼로 감싼다.

`/login`과 `/signup`은 반대: 이미 인증된 사용자가 오면 `/`로 redirect.

### 2.5 에러 처리

- API 401 → 자동 `/login` 리다이렉트 (TanStack Query global onError)
- API 4xx → toast (shadcn `useToast`)로 `{"error": "..."}` 문자열 표시
- API 5xx → toast "internal error, try again"
- 네트워크 실패 → 같은 toast

### 2.6 SSH 명령 카드 (env detail)

env status가 `running`일 때만 표시. 두 명령 각각 별도 Copy 버튼:

```
┌──────────────────────────────────────────────┐
│ Connect via SSH                              │
│ ┌──────────────────────────────────────────┐ │
│ │ flexctl ssh cuda                         │ │  [📋 Copy]
│ └──────────────────────────────────────────┘ │
│ or:                                          │
│ ┌──────────────────────────────────────────┐ │
│ │ ssh dev@paul-cuda.flex                   │ │  [📋 Copy]
│ └──────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```

## 3. 백엔드 변경

### 3.1 새 API: `GET /v1/nodes`

**Why**: env create 폼의 노드 dropdown을 채우기 위해. Plan 3의 `nodes.Service`는 있지만 list 핸들러는 미노출.

**Spec**:
- 핸들러: `internal/nodes/handlers.go`에 추가
- Mount: `r.Get("/v1/nodes", h.list)` (session auth 안)
- 동작: `nodes.Service.ListByOwner(uid)` (없으면 추가) — 사용자가 join한 노드만. 다른 사용자 노드 격리.
- 응답: `[{"id":"<uuid>","name":"h20a","status":"online","gpu_info":{...},"last_seen_at":"..."}]`
- 빈 배열 OK

**TDD**: httptest로 ownership 격리 (alice 노드가 bob의 GET에 안 보임) + 빈 배열 정상 반환.

### 3.2 SPA serving

**`internal/webui/embed.go`**:

```go
package webui

import (
    "embed"
    "io/fs"
)

//go:embed all:dist
var distFS embed.FS

func Assets() fs.FS {
    sub, err := fs.Sub(distFS, "dist")
    if err != nil { panic(err) } // build invariant
    return sub
}
```

Vite의 `outDir`을 `../internal/webui/dist`로 설정해 직접 거기 빌드. `web/dist/` 별도 디렉토리 없음.

**control-plane `cmd/control-plane/main.go` 변경**:
- chi router에 SPA 핸들러를 모든 API 라우트 뒤에 mount:
  1. `/v1/*`, `/healthz` → 기존
  2. 그 외 GET → SPA 핸들러

```go
spaHandler := spa.New(webui.Assets())
r.NotFound(spaHandler.ServeHTTP)
```

### 3.3 SPA fallback 핸들러 — `internal/spa/handler.go` (신규)

```go
package spa

import (
    "io"
    "io/fs"
    "net/http"
    "strings"
    "time"
)

type Handler struct {
    fs       fs.FS
    indexBuf []byte
}

func New(assets fs.FS) *Handler {
    idx, err := fs.ReadFile(assets, "index.html")
    if err != nil { panic("index.html not found in embedded SPA assets") }
    return &Handler{fs: assets, indexBuf: idx}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet && r.Method != http.MethodHead {
        http.NotFound(w, r); return
    }
    p := strings.TrimPrefix(r.URL.Path, "/")
    if p == "" { p = "index.html" }
    f, err := h.fs.Open(p)
    if err != nil {
        // SPA route — serve index.html so React Router takes over
        w.Header().Set("Cache-Control", "no-store")
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(h.indexBuf); return
    }
    defer f.Close()
    if strings.HasPrefix(p, "assets/") {
        // Vite outputs hashed asset filenames — safe to cache aggressively.
        w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
    }
    rs, ok := f.(io.ReadSeeker)
    if !ok { http.Error(w, "embedded FS not seekable", 500); return }
    http.ServeContent(w, r, p, time.Time{}, rs)
}
```

**단위 테스트** (`internal/spa/handler_test.go`): in-memory `fs.MapFS`로 `index.html` + `assets/main.abc.js` 두 파일,
- (a) 존재하는 path → 정확한 content + assets/ prefix면 immutable cache header
- (b) `/envs/new` → index.html 폴백 + `no-store`
- (c) POST 메서드 → 405/404 (변경 메서드는 거부)

### 3.4 CSRF / Origin 체크

**`internal/auth/middleware.go`에 `RequireSameOrigin`** 추가:

```go
// allowedOrigins: 빈 슬라이스면 dev/test용 — 검사 건너뜀.
func RequireSameOrigin(allowed []string) func(http.Handler) http.Handler { ... }
```

env: `FLEX_ALLOWED_ORIGINS` (comma-separated). 비어있으면 dev 모드로 검사 skip. POST/PUT/DELETE 요청에서 `Origin` 헤더가 allowed 목록에 없으면 403. `RequireSession` 뒤에 chain.

### 3.5 변경 요약 (백엔드)

| 파일 | 변경 |
|---|---|
| `internal/nodes/service.go` | `ListByOwner(ctx, userID)` 추가 (없으면) |
| `internal/nodes/handlers.go` | `r.Get("/v1/nodes", h.list)` + 핸들러 |
| `internal/auth/middleware.go` | `RequireSameOrigin` 추가 |
| `internal/spa/handler.go` | 신규 |
| `internal/webui/embed.go` | 신규 |
| `cmd/control-plane/main.go` | spa 핸들러 mount + `RequireSameOrigin` middleware + `FLEX_ALLOWED_ORIGINS` 처리 |

## 4. 빌드 / 테스트 파이프라인

### 4.1 Vite 프로젝트 셋업

```bash
cd web/
npm create vite@latest . -- --template react-ts
npm i
npm i -D tailwindcss postcss autoprefixer
npx tailwindcss init -p
npm i react-router-dom @tanstack/react-query
```

**`web/vite.config.ts`**:

```ts
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: { '/v1': 'http://localhost:8080' },
  },
})
```

dev: `npm run dev`는 5173, `/v1`은 8080 proxy. prod: `npm run build`가 `internal/webui/dist`에 산출물.

### 4.2 Tailwind + shadcn 통합

```bash
# Tailwind config
cat > tailwind.config.js <<'EOF'
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: { extend: {} },
  plugins: [],
}
EOF

# shadcn CLI — slate theme, CSS vars
npx shadcn-ui@latest init

# Plan 6 범위에 필요한 8개 컴포넌트만 vendor:
npx shadcn-ui@latest add button input label card badge table toast dialog
```

shadcn은 NPM dep이 아니라 **컴포넌트 코드를 `src/components/ui/`에 vendor**한다. radix-ui + cva 정도만 NPM dep. vendor 코드는 git commit.

### 4.3 Playwright e2e

```bash
cd web/
npm i -D @playwright/test
npx playwright install chromium    # MVP는 chromium 1개만
```

**`web/playwright.config.ts`**:

```ts
export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  use: { baseURL: 'http://localhost:8080' },
  webServer: {
    command: '../bin/control-plane',
    url: 'http://localhost:8080/v1/health',
    env: {
      FLEX_SESSION_SECRET: 'e2e-secret-32-bytes-AAAAAAAAAAAAAAA',
      FLEX_DB_DSN: 'postgres://flex:flex@localhost:5433/flex?sslmode=disable',
      FLEX_HEADSCALE_URL: 'http://localhost:8090',
      FLEX_HEADSCALE_API_KEY: '${HS_KEY}',
      FLEX_E2E_AUTOACK: '1',  // dispatcher가 즉시 EnvReady 자가 콜백
    },
    timeout: 30_000,
  },
})
```

**`web/e2e/golden-path.spec.ts`** — 메인 스펙 §8 요구사항 충실 반영:

```ts
test('signup → create env → SSH command visible', async ({ page }) => {
  await page.goto('/signup')
  await page.fill('input[name="email"]', `test-${Date.now()}@x.com`)
  await page.fill('input[name="slug"]', `tester${Date.now()}`)
  await page.fill('input[name="password"]', 'supersecret123')
  await page.click('button[type="submit"]')
  await page.waitForURL('/')

  await expect(page.locator('text=No environments yet')).toBeVisible()
  await page.click('text=New env')
  await page.waitForURL('/envs/new')

  await page.fill('input[name="name"]', 'cuda')
  await page.selectOption('select[name="template_id"]', 'cuda-base')
  await page.selectOption('select[name="node_id"]', { index: 0 })
  await page.fill('input[name="gpu_request"]', '1')
  await page.click('button[type="submit"]')
  await page.waitForURL(/\/envs\/[\w-]+/)

  await expect(page.locator('text=running')).toBeVisible({ timeout: 30_000 })
  await expect(page.locator('text=flexctl ssh cuda')).toBeVisible()
  await expect(page.locator(/ssh dev@.*\.flex/)).toBeVisible()
})
```

### 4.4 E2E를 위한 dispatcher autoack mode

실제 Docker + sidecar + dev 컨테이너를 e2e에서 띄우는 건 너무 무거움. 대신 control-plane에 dev-only 플래그 `FLEX_E2E_AUTOACK=1`을 추가:
- envs.Dispatcher.Create 호출 시 실제 agent에 송신하지 않고, 짧은 delay(0.5s) 후 envs.Service.MarkRunning을 자가 호출.
- Stop/Start/Delete도 마찬가지로 즉시 종착 상태 적용.
- 이 모드에서는 GPU 노드도 mock으로 미리 DB에 1건 seed (e2e setup script가 처리).

Plan 6 본 코드는 production path만 — autoack은 environment-gated branch. e2e 외에는 절대 켜지 않음.

### 4.5 빌드 / Makefile

```make
.PHONY: web-dev web-build build-control-plane e2e e2e-up e2e-down

web-dev:
	cd web && npm run dev

web-build:
	cd web && npm ci && npm run build

# control-plane 빌드 전에 web/dist를 만들어야 //go:embed가 동작
build-control-plane: web-build
	go build -o bin/control-plane ./cmd/control-plane

e2e-up:
	docker compose -f docker-compose.e2e.yml up -d
	./scripts/e2e-headscale-init.sh > .env.e2e

e2e-down:
	docker compose -f docker-compose.e2e.yml down -v

e2e: build-control-plane e2e-up
	cd web && npx playwright test
```

`.gitignore`에 `internal/webui/dist/` 추가.

### 4.6 에러 / 실패 모드

| 시나리오 | 처리 |
|---|---|
| `internal/webui/dist/` 없음 | `go build`가 명확한 에러: "pattern dist: no matching files". Makefile이 `web-build` 의존성으로 막음 |
| API 401 → 자동 /login 리다이렉트 | TanStack Query global error handler |
| GET /v1/nodes 빈 배열 | env create 폼에서 "join a node first" + `flexctl join` 명령 hint |
| Playwright e2e에서 autoack mode 미설정 | env가 영원히 creating → 30s timeout 후 명확한 실패 |
| shadcn add 실행 안 됨 (오프라인) | vendor된 컴포넌트는 git에 commit되므로 다음부터는 npm dep만으로 충분 |

### 4.7 보안

- `Origin` 헤더 체크(`RequireSameOrigin`)로 CSRF 방어. 운영 시 `FLEX_ALLOWED_ORIGINS=https://flexctl.example.com` env로 강제.
- session cookie는 이미 HttpOnly + SameSite=Lax (Plan 2).
- 사용자 입력은 React가 기본 escape. shadcn 컴포넌트는 dangerouslySetInnerHTML 사용 안 함.
- env hostname/SSH 명령 표시 시 React 기본 escape로 충분.

## 5. 범위 밖 (Plan 7+)

- GitHub OAuth (이메일/비번만 — 메인 스펙 §6.1)
- 비밀번호 재설정 / 이메일 확인
- 사용량 측정 / 대시보드 차트 (메인 스펙 §9)
- 다국어
- 다크 모드 (shadcn은 지원하지만 MVP는 light only)
- 모바일 최적화 (desktop-first)
- 실시간 알림 (env 상태 변경 push) — MVP는 3초 polling
- /nodes 페이지 (env create dropdown으로 충분)
- /settings 페이지 (프로필/비밀번호 변경)
- 노드 공유 (메인 스펙 §9)
- 결제/사용량 측정 (메인 스펙 §9)
