CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email           text NOT NULL UNIQUE,
  slug            text NOT NULL UNIQUE,
  password_hash   text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX users_email_lower_idx ON users (lower(email));
