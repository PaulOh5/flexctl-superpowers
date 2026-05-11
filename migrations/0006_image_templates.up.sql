CREATE TABLE image_templates (
  id            text PRIMARY KEY,
  display_name  text NOT NULL,
  description   text NOT NULL DEFAULT '',
  image_ref     text NOT NULL,
  default_cmd   text[] NOT NULL DEFAULT '{}',
  enabled       bool NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now()
);
