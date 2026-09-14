-- 0001_init.sql — roles, policies, users, sessions, audit.
--
-- Postgres is the source of truth for the query path: every read resolves a caller's roles and
-- policies from these tables, never from the config file. The YAML writes them at boot.
--
-- ClickHouse holds no authorization state. Nothing here mirrors trace data.

create extension if not exists pgcrypto;   -- gen_random_uuid()

-- 'user' is a reserved word in SQL; the table is app_user everywhere.
create table app_user (
  id            uuid primary key default gen_random_uuid(),
  username      text        not null unique,
  password_hash text        not null,      -- argon2id; never a plaintext password
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);

-- Attributes are a privilege boundary: whoever sets tenant_id decides what that user sees.
-- `source` records where a value came from, so v2's IdP mapping has a stored fact to resolve
-- precedence against rather than a guess.
create table user_attribute (
  user_id uuid not null references app_user(id) on delete cascade,
  key     text not null,
  value   text not null,
  source  text not null default 'config',  -- config | api | idp
  primary key (user_id, key)
);

create table role (
  id   uuid primary key default gen_random_uuid(),
  name text not null unique
);

-- One role carries one or more policies. Policies OR together across a caller's roles, so the
-- model is monotonic: holding another role can only widen what a caller sees.
create table policy (
  id          uuid primary key default gen_random_uuid(),
  role_id     uuid not null references role(id) on delete cascade,
  title       text not null,
  description text,
  expression  text not null,               -- CEL, stored as written
  unique (role_id, title)
);

create table role_assignment (
  user_id uuid not null references app_user(id) on delete cascade,
  role_id uuid not null references role(id) on delete cascade,
  primary key (user_id, role_id)
);

-- Sessions are rows rather than stateless signed cookies: revocation has to be possible, and a
-- deprovisioned caller must lose access without waiting for a token to expire.
create table session (
  token_sha  text        primary key,      -- sha256 of the token; the token itself is never stored
  user_id    uuid        not null references app_user(id) on delete cascade,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);
create index on session (user_id);
create index on session (expires_at);

-- The audit log outlives the users it describes, so the caller is denormalised. A deleted user
-- must not take their query history with them.
create table audit_log (
  id             bigserial   primary key,
  at             timestamptz not null default now(),
  username       text        not null,
  action         text        not null,     -- auth.login | query.list | query.detail | attribute.set
  policy_filters text,                     -- the CEL applied, as written
  detail         jsonb       not null default '{}'
);
create index on audit_log (at desc);
create index on audit_log (username, at desc);
