#!/usr/bin/env bash
set -euo pipefail

docker compose -f docker-compose.e2e.yml exec -T headscale-e2e \
  headscale users create control-plane 2>/dev/null || true

KEY=$(docker compose -f docker-compose.e2e.yml exec -T headscale-e2e \
  headscale apikeys create --expiration 1d | tail -1)

cat > .env.e2e <<EOF
FLEX_ADDR=:8080
FLEX_GRPC_ADDR=:9091
FLEX_DB_DSN=postgres://flex:flex@localhost:5433/flex?sslmode=disable
FLEX_SESSION_SECRET=e2e-secret-32-bytes-AAAAAAAAAAAAAAAA
FLEX_HEADSCALE_URL=http://localhost:8090
FLEX_HEADSCALE_API_KEY=$KEY
FLEX_HEADSCALE_CLIENT_URL=http://localhost:8090
FLEX_TAILNET_DOMAIN=flex
FLEX_SIDECAR_IMAGE=flex/sidecar:dev
FLEX_E2E_AUTOACK=1
EOF
echo "wrote .env.e2e"
