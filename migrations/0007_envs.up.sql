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
