CREATE TABLE nodes (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name            text NOT NULL,
  agent_version   text NOT NULL DEFAULT '',
  gpu_info        jsonb NOT NULL DEFAULT '[]'::jsonb,
  status          text NOT NULL DEFAULT 'offline',
  node_token_hash text NOT NULL UNIQUE,
  last_seen_at    timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE(owner_user_id, name)
);

CREATE INDEX nodes_status_idx ON nodes (status);
