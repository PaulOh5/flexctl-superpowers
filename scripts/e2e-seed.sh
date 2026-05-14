#!/usr/bin/env bash
# Inserts one fake "e2e-node" for a given user slug so /v1/nodes is non-empty.
set -euo pipefail

USER_SLUG="${1:?usage: e2e-seed.sh <user-slug>}"
PSQL="docker compose -f docker-compose.e2e.yml exec -T postgres-e2e psql -U flex -d flex -t"

USER_ID=$($PSQL -c "SELECT id FROM users WHERE slug='$USER_SLUG'" | xargs)
$PSQL -c "
INSERT INTO nodes (owner_user_id, name, agent_version, gpu_info, status, node_token_hash)
VALUES ('$USER_ID', 'e2e-node', '0.1.0-e2e', '{}'::jsonb, 'online', 'e2e-hash-$USER_ID')
ON CONFLICT DO NOTHING;
"
echo "seeded e2e-node for $USER_SLUG"
